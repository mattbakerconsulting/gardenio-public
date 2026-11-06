# MicroVM resume rehydration — the correctness boundary (step 5)

**Plan:** MicroVM native suspend/resume (bundle `ka-0000004518`, epic `ka-0000004689`).
**Task:** step 5 — resume rehydration (Ziti/NATS/creds correctness boundary), `ka-0000001608`.
**Reference precedent:** the gVisor thaw rehydration design (`gardenio/docs/gvisor-runsc-cr/rehydration-design.md`,
`ka-0000001515`) and its controller-side fail-closed rendezvous
(`gardenio/controller/internal/podmanager/freezethaw_rehydrate.go`).

This is the correctness boundary the plan flags as **HIGHEST RISK**: after `ResumeMicrovm`, the
resumed agent must never serve the user on a half-rehydrated identity (stale Ziti overlay, dead NATS
4-tuples, cached endpoints). We build the MicroVM analog of the gVisor fail-closed thaw-epoch
rendezvous — but the mechanism is fundamentally different, and understanding *why* is the whole point
of this note.

---

## 0. TL;DR — why the MicroVM boundary is NOT the gVisor boundary

The gVisor thaw resumes a **frozen process tree**: `main()` never re-runs, so every in-RAM credential
and every long-lived TCP 4-tuple is **stale** (bound to Pod A). Rehydration there is *cache
invalidation + deterministic reconnect inside a living process*, driven by a controller-written
marker on a hostPath the in-pod watchers poll, with the controller polling their acks back over the
same hostPath (`NodeExecutor`). Fail-closed = don't release until both `gb` and `coop` ack OK.

**None of that plumbing exists for a MicroVM, and none of it is needed.** Two hard topology facts
reshape the design:

1. **`ResumeMicrovm` is identifier-only.** `ResumeMicrovmInput` carries *only* `MicrovmIdentifier`
   (see the AWS SDK `lambdamicrovms` v1.1.1). The controller **cannot inject a payload** — no
   marker, no epoch, no pod-uid — into the VM on resume.
2. **MicroVMs are egress-only.** The backend configures only the `INTERNET_EGRESS` connector
   (`microvm/backend.go` `internetEgressConnectorARN`); there is no ingress connector, no hostPath,
   and no `NodeExecutor`. The controller **cannot dial into the VM** to write a marker or read an
   ack, and AWS auto-resume-on-inbound cannot fire (nothing dials in) — which is exactly why resume
   is controller-driven in the first place (step 3).

So the gVisor rendezvous (controller pushes signal in → agent acks back over a shared hostPath) is
structurally impossible here. **But the signal channel already exists natively**, and it is
*stronger* than the gVisor marker.

---

## 1. The signal channel: the AWS resume lifecycle hook (a fresh warm-boot)

The MicroVM agent image (`gardenio/images/microvm-agent/entrypoint.py`) runs an **in-VM lifecycle HTTP
server** on port 8080 that AWS drives with run-hooks:
`POST /aws/lambda-microvms/runtime/v1/{ready,validate,run,resume,suspend,terminate}`.

On `ResumeMicrovm`, **AWS fires the in-VM `resume` hook**, and the entrypoint dispatches
`hook in ("run", "resume") → RuntimeManager.start()`, which:

1. `_stop_locked()` — terminates the old OpenZiti proxy + agent process **and unlinks the stale
   identity / NATS-token / claude-creds / SSH-key files**;
2. `_write_identity()` — re-reads the per-run Ziti identity secret from Secrets Manager (reusing the
   already-enrolled identity on disk — the one-time JWT is consumed only on first boot);
3. `_load_agent_secret()` — re-reads the agent-config secret **fresh** (fresh NATS token, Claude
   credentials, git creds, env);
4. `_start_ziti()` — restarts the OpenZiti userspace proxy → **all overlay connections are
   re-established fresh**;
5. `_start_agent()` — restarts `entrypoint.sh → gb agent start` → **the agent process boots again,
   so boot-time init re-runs** (unlike gVisor, `main()` DOES run again).

**Therefore: MicroVM "rehydration" is a fresh warm-boot, not a stale-RAM poison-and-reconnect.** The
resume hook already IS the "you were resumed; re-credential + reconnect" signal the task asks us to
send — and because the whole userspace restarts, there is *no* per-subsystem stale-RAM hazard to
poison. The four gVisor stages (poison creds / re-read tokens / reconnect NATS / re-announce) all
collapse into "the process restarted and read everything fresh."

What the controller CANNOT assume is that this restart **succeeded**. The resume hook can 503
(Secrets Manager unreachable, Ziti enrollment/re-load failed, agent crash on boot), the fresh boot
can fail to re-establish the overlay, or the Ziti identity lease can have expired during standby
(§4). That is the residual correctness risk step 5 must close.

---

## 2. The correctness contract (what "rehydrated" means for a MicroVM)

A MicroVM resume is **correct** iff, before the agent is marked live (`agent_state=working`, which is
what unblocks the user's message reaching it), the resumed VM has **proven** it re-established its
external identity and connectivity. We express this as a single checkable post-condition, because for
a MicroVM one signal proves the whole contract at once:

**M1 — Proof-of-life over the overlay.** The resumed in-VM `gb agent start` emits `SessionStart` (and
then a heartbeat) that **egresses over the freshly-enrolled Ziti overlay, using the freshly-read NATS
token, to reach the beads daemon** and advance the agent's presence `last_seen`
(`kbeads/internal/presence`, surfaced via `GET /v1/agents/roster`). **The arrival of that heartbeat
at beads is proof that identity, overlay connectivity, and beads reachability are ALL live** — if any
were broken, the heartbeat could never have egressed. This single egress ack subsumes the gVisor
C1–C4 (identity re-minted, tokens re-read, connections re-established, presence re-announced), because
a fresh MicroVM boot cannot *partially* satisfy them: either the overlay + creds work and the
heartbeat lands, or they don't and it never arrives.

**M2 — Bounded and fail-closed (the gVisor C5 analog).** The wait for M1 runs under a deadline
(`MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS`, default 90s). If proof-of-life does not arrive in time,
the resume **fails closed**: the controller does **not** mark the agent live, it terminates the
un-rehydratable VM (revoking its per-run secrets + Ziti lease), and stamps `agent_state=resume_failed`
so the next message cold-starts a fresh VM. The agent never resumes serving the user on an ambiguous
identity.

Why 90s (vs the gVisor 10s): a MicroVM resume restarts the *entire userspace* — process spawn, Ziti
proxy re-tunnel, Secrets Manager reads, STS, and the first heartbeat round-trip over the overlay —
which is meaningfully slower than a gVisor in-process reconnect. The VM disk survives suspend so there
is **no** cold repo re-clone (the dominant gVisor create-path cost), but the boot + overlay re-dial
still wants generous headroom. 90s is comfortably under the 15-min presence reaper.

---

## 3. Architecture: the controller-side fail-closed rendezvous

```
 gb stop (step 2) ── SuspendMicrovm ──▶ VM SUSPENDED, bead OPEN @ agent_state=stopping, creds/lease intact
                                                              │
 next message (step 3) ── ResumeMicrovm ──▶ AWS fires in-VM `resume` hook ──▶ RUNTIME.start():
                                                              │        re-read secrets, restart ziti + agent (fresh boot)
                                                              ▼
        ╔══════════════ RESUME REHYDRATION RENDEZVOUS (this step, microvm/rehydrate.go) ═══════════════╗
        ║  Resume():           backend.Resume (idempotent) → stamp agent_state=resuming (NOT working)  ║
        ║  AwaitRehydration(): poll presence proof-of-life (M1) until last_seen advances past resumeAt ║
        ║      proof  within deadline → stamp agent_state=working   (the VM is live; message may flow) ║
        ║      timeout (fail closed) → Terminate VM + stamp agent_state=resume_failed  (M2)            ║
        ╚═══════════════════════════════════════════════════════════════════════════════════════════════╝
                                                              │
                    working → the resumed agent pulls/serves the user's message over the live overlay
                    resume_failed → next message finds a non-stopping bead → bridge cold-starts fresh
```

- **Signal in** = the AWS resume hook (already restarts the agent). No marker write — impossible and
  unnecessary.
- **Ack back** = the resumed agent's own egress heartbeat reaching beads (presence `last_seen`). This
  is the MicroVM analog of the gVisor `.done` ack files, but it rides the *product* egress path
  rather than a hostPath, so it doubles as an end-to-end overlay+creds health assertion.
- **Fail-closed gate** = `agent_state` never reaches `working` without proof; on timeout the VM is
  terminated and the bead marked `resume_failed`.

### Where the code lives

- `microvm/rehydrate.go` — `LivenessProber` seam + the in-flight registry + `AwaitRehydration` (the
  bounded fail-closed proof-of-life gate). Kept free of the beads-daemon dependency (like
  `AgentBeadView`): the prober is an interface the controller wires with a `GetAgentRoster` adapter.
- `microvm/lifecycle.go` — `Resume` now stamps `agent_state=resuming` (gated) instead of `working`;
  `AwaitRehydration` owns the `working` / `resume_failed` transition.
- `cmd/controller` — `POST /api/v1/microvm/resume` issues the synchronous `Resume` (reflecting AWS
  success in the HTTP response so a backend failure still falls the bridge through to cold-start),
  then launches `AwaitRehydration` on a **detached** context (the rendezvous can outlast the bridge's
  10s HTTP client), and `GET /api/v1/microvm/resume/status?agent=` reports the in-flight/terminal
  outcome (mirrors `GET /api/v1/thaw/status`).
- `internal/config` — `MICROVM_RESUME_REHYDRATE_ENABLED` (default on; off = the step-3
  stamp-working-immediately rollback) and `MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS` (default 90s).

---

## 4. Ziti identity lease freshness across suspend (broker TTL vs standby age)

The per-run Ziti identity is leased from the ziti-access-broker with
**TTL = `MaximumDurationInSeconds` + `leaseTTLSlack` (15m)** (`microvm/broker.go`), and revoking a
lease *disables + deletes* the overlay identity at the Ziti controller. So:

> **The Ziti lease — not the session credentials — is the binding standby constraint.** Session creds
> default to 12h (`MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS`, `config.go`), but the Ziti lease
> lives only `maxDuration + 15m`. A VM suspended longer than its lease lifetime resumes with a
> **revoked identity**: the OpenZiti proxy can no longer dial the overlay, so *nothing* — not beads,
> not NATS, not LiteLLM — is reachable.

**How this design handles it:** the fail-closed proof-of-life gate (M1/M2) catches a dead lease
**automatically and correctly** — a resumed VM whose identity was revoked cannot egress a heartbeat,
so proof-of-life times out and the resume fails closed to a clean cold-start. No stranded zombie VM,
no half-rehydrated identity served to the user. The gate is the single enforcement point for *every*
rehydration failure mode, dead-lease included.

A **proactive** standby-age guard (compare `now − suspended_at` against the lease TTL and skip a
doomed resume, cold-starting immediately) is a pure latency optimization — it saves one ~90s
fail-closed wait on the rare over-long standby. It is deliberately **out of step-5 scope** because it
requires persisting `suspended_at` on the bead (a step-2/step-4 field the suspend path does not write
today) and because the reactive gate is already correct without it. Tracked for the cost/metrics step
(step 6 `ka-0000002861`): if telemetry shows long standbys are common, persist `suspended_at` at
suspend time and add the pre-resume age check. The safe default meanwhile is to keep
`MaximumDurationInSeconds` (hence the lease TTL) generously sized relative to expected standby, or to
lean on the `IdlePolicy.SuspendedDurationSeconds` auto-terminate backstop so an over-long standby is
reaped rather than resumed onto a dead lease.

---

## 5. Failure-mode matrix

| Failure | Detection | Response |
|---|---|---|
| `backend.Resume` (AWS `ResumeMicrovm`) errors | synchronous error from `Resume` | endpoint returns 500 → bridge falls through to cold respawn; resume is idempotent so a retry is safe |
| Resume hook 503 (Secrets Manager / enrollment / agent crash on boot) | no heartbeat egresses → M1 never proven | **fail closed (M2)**: terminate VM + `agent_state=resume_failed`; next message cold-starts |
| Ziti lease expired during standby (§4) | overlay dial fails → no heartbeat → M1 never proven | **fail closed (M2)** — identical path; the gate is lease-agnostic |
| Fresh boot slow but healthy | heartbeat lands before deadline | proof (M1) → `agent_state=working`; the 90s budget absorbs boot + overlay + STS + first heartbeat |
| Duplicate resume / race with reconciler | `backend.Resume` idempotent (ConflictException tolerated); in-flight registry dedups `AwaitRehydration` | second resume is a no-op; one rendezvous per agent |
| Rehydration disabled (rollback) | `MICROVM_RESUME_REHYDRATE_ENABLED=false` | `Resume` keeps step-3 behavior (stamp `working` immediately, no gate) |
| Proof-of-life false timeout (transient beads blip) | deadline expires despite a healthy VM | fail closed + cold-start (correctness over VM preservation, matching gVisor "abort thaw, discard Pod B"); 90s budget + 1s polling makes this rare |

---

## 6. New `agent_state` values

- `resuming` — set by `Resume` the moment `backend.Resume` succeeds, held for the duration of the
  rehydration rendezvous. Not in `deadAgentStates`, not `working`, not a startup state — it reads as
  "coming back, not yet live," which is exactly correct. The bridge's resume-in-place guard keys on
  `agent_state == "stopping"`, so a `resuming` bead is (correctly) NOT re-resumed by a concurrent
  message; the in-flight registry backstops the dedup.
- `resume_failed` — the fail-closed terminal-ish state. Not `stopping`, so the next message falls
  through the bridge's resume-in-place guard to a normal cold respawn (which closes the bead and
  launches fresh). The step-4 reconciler also converges it (terminal runtime → close).

---

## 7. Verification

Unit tests (this MR):
- `Resume` (gated) stamps `resuming` not `working`; disabled keeps step-3 `working`; idempotent /
  nil-backend no-ops.
- `AwaitRehydration`: proof-before-deadline → `working`; timeout → `Terminate` + `resume_failed`;
  fake `LivenessProber` drives both; in-flight registry set/cleared; idempotency.
- Resume endpoint: `resumed:true,rehydrating:true` on success; backend error → 500; every
  not-applicable miss (backend off / not a MicroVM / not suspended / no runtime_id) → 200
  `resumed:false`; resume-status endpoint reports in-flight vs settled.

E2E (step 6, `ka-0000002861`): a real suspend→resume cycle asserting (a) a healthy resume reaches
`working` only after the resumed VM's heartbeat lands, and (b) an induced rehydration failure (e.g. a
revoked lease / blocked overlay) fails closed to `resume_failed` + cold-start — the on-substrate
analog of the gVisor `test-gvisor-rehydration.sh` contract assertions.

---

## 8. Verdict

The gVisor rehydration boundary closes a *stale-RAM-in-a-living-process* hazard with a
controller-pushed marker and a hostPath ack rendezvous. The MicroVM boundary is a different animal:
`ResumeMicrovm` is identifier-only and the VM is egress-only, so nothing can be pushed in — but the
AWS resume hook already restarts the whole userspace fresh, so there is no stale RAM to poison. The
residual risk is that the *fresh boot* (or the Ziti lease) may have failed. Step 5 closes that with a
controller-side, fail-closed **proof-of-life rendezvous**: mark the agent live only once its own
egress heartbeat has re-crossed the overlay to beads, else terminate and cold-start. One egress ack
proves identity + overlay + reachability at once, and the same gate catches an expired Ziti lease for
free.
