# Design: credential & NATS re-hydration on agent thaw (correctness boundary)

**Bead:** ka-0000001515 (P1) · **Epic:** ka-0000006959 (Phase 3) · **Substrate epic:** ka-0000003796
**Upstream (proven):** restore-through-lifecycle ka-0000003405 (`restore-through-lifecycle.md`, MR !3488)
**Inputs:** Phase-2 benchmark ka-0000004928 (`report.md`), recon ka-0000003467 (`phase3-cri-recon.md`),
prior-art ka-0000004052 (`prior-art.md`)
**Date:** 2026-06-04 · **Status:** design (no code change in this MR)

---

## 0. TL;DR — what this design fixes and why it is a *correctness* boundary

The restore-through-lifecycle work (ka-0000003405) proved a checkpointed gVisor agent can be
thawed **into a fresh, kubelet-owned pod sandbox** — kubelet re-provisions a new CNI netns, new
emptyDir volumes, and a freshly-minted projected SA token; the old pod's bindings are GC'd. The
sandbox sentry and the new pod share one netns inode; thaw is sub-300 ms.

But the *process inside that sandbox is still living in the past.* Its guest RAM (`pages.img`) is a
byte-for-byte copy of the moment of freeze. Every piece of **external identity and every live
connection** the agent cached in memory before freeze is now **stale or dead**:

| What the agent cached in RAM at freeze | State after thaw onto a new pod/node |
|---|---|
| IRSA web-identity token contents (if already read) | **expired / bound to Pod A's SA** — STS exchange will 403 |
| projected SA token contents (if already read) | **expired / bound to Pod A** — beads/k8s API calls 401 |
| beads bearer / AVA session cookie | possibly still valid, possibly rotated under it |
| Anthropic / account API key | usually still valid (long-lived secret) but may have rotated |
| NATS TCP connection (4-tuple to old pod IP) | **dead on first write** — restored FD points at a 4-tuple that no longer routes |
| `KUBERNETES_SERVICE_HOST/PORT` env, in-RAM DNS | baked at create time — **stale**, point at A's view |

This is a **correctness boundary**, not a perf optimization: a thawed agent that does *not*
rehydrate will appear alive (the process runs, RAM is intact) but will **silently fail its next
authenticated API call and its next NATS publish**, and will never re-appear in the roster. Worse,
if a token *happens* to still be valid for a few minutes, the agent acts under **Pod A's identity**
inside Pod B — an audit/identity-confusion hazard. The contract below makes thaw a **warm boot**:
keep RAM + FS, but **discard and re-establish every external identity and connection** before the
agent is allowed to resume issuing work.

The model is AWS Lambda **SnapStart**'s post-restore hook contract (prior-art §10–12): the snapshot
is restored, then a *runtime hook* fires that re-seeds anything time- or identity-sensitive (RNG,
DB connections, credentials) **before** the handler is allowed to serve a request.

---

## 1. The correctness contract (what "rehydrated" means)

A thaw is **correct** iff, before the agent's main loop issues its next outbound request, all of the
following hold. Each is a checkable post-condition (see §6 verification).

**C1 — No Pod-A identity is ever used under Pod B.**
Every credential the agent presents after thaw must derive from a token **kubelet minted for Pod B**,
or from a long-lived secret re-read from disk after thaw. The in-RAM copies from Pod A are treated as
**poisoned** and discarded.

**C2 — Credentials are re-minted, never restored.**
Short-lived tokens (IRSA web-identity, projected SA) are **re-read from their on-disk paths** (which
kubelet has repopulated for Pod B) and any in-process credential cache is invalidated so the next use
forces a fresh STS / token exchange. We never carry a token value across the freeze line.

**C3 — All long-lived connections are re-established.**
Every NATS connection (publisher, relay pub+sub, forwarder, inbox JetStream consumer) and any other
long-lived TCP is **torn down and reconnected** to the current endpoint with a current token. A
restored TCP FD bound to A's 4-tuple is never written to.

**C4 — Presence is re-announced.**
After reconnect the agent re-announces itself (heartbeat + relay `announce`) so coopmux / the
presence tracker re-discover it. A thawed agent that is invisible to the roster is, operationally, a
dead agent.

**C5 — Rehydration is bounded and fail-closed.**
The whole sequence runs under a deadline (proposed **10 s**, see §5). If any *required* step
(C1–C4 minus best-effort items) does not complete, the agent **does not silently continue** — it
surfaces the failure (the thaw is aborted / the pod is failed) rather than running half-rehydrated
under ambiguous identity.

**Non-goals.** This design does *not* cover: getting the process tree back (that's
ka-0000003405 + the open sub-container `mounts-fd` item ka-0000000845); snapshot transport between
nodes (recon open item); in-flight Anthropic HTTP request resumption (the Claude CLI's own retry
handles a dropped request — we only guarantee creds/connectivity are valid for the *next* attempt).

---

## 2. Inventory: every external dependency that crosses the freeze line

Grounded in the agent image (`gardenio/images/agent/entrypoint.sh`, `resolve-auth.sh`), the spawn
path (`gardenio/controller/cmd/gb/agent_start*.go`), and coop's transports
(`coop/crates/cli/src/transport/nats*.rs`, `run.rs`).

### 2.1 Credentials / identity

| # | Credential | How it's sourced at *boot* today | Lifetime | Thaw action |
|---|---|---|---|---|
| K1 | **IRSA web-identity token** (`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`) | EKS projects the token file into the pod; AWS SDK reads it lazily and exchanges via STS AssumeRoleWithWebIdentity (advice ka-0000001739) | token ~ hours, STS creds ~15 min–1 h | **re-read file path** (kubelet repopulated for B); invalidate SDK's in-memory STS cache so next AWS call refreshes |
| K2 | **Projected SA token** (`/var/run/secrets/kubernetes.io/serviceaccount/token` and `…/kube-api-access-XXX/token`) | kubelet projects + rotates; `kd`/k8s clients read it per-call | ~1 h, kubelet-rotated | **re-read file path**; bounce any client holding a cached bearer |
| K3 | **beads bearer / AVA session cookie** | `entrypoint.sh` resolves via daemon connect; cookie from `gb login` → credstore; consumed by `kd`, `gb`, and the coop **nats-bridge** sidecar (`nats_bridge.go`) | bearer hours; AVA cookie session-bound | re-read from credstore / env; if the bridge holds a stale cookie, restart the bridge (see §4.3) |
| K4 | **Account / Anthropic credentials** (`.credentials.json` at `$CREDS_PVC`, `ANTHROPIC_BASE_URL`, `CLAUDE_MODEL`) | `resolve-auth.sh::_resolve_account_auth` writes from account bead via coopmux/secret/env | long-lived secret (rotates rarely) | re-run account resolution **only if** the on-disk file is older than the freeze or a rotation marker changed; otherwise leave (Claude CLI reads it per-request) |
| K5 | **git credentials** (`writeGitCredentials`, `agent_k8s_workspace.go`) | derived from `BOAT_PROJECTS` + injected GitLab token at workspace setup | token long-lived | best-effort re-write; only matters if the agent pushes after thaw |

**Key insight (from ka-0000004928 comment + checkpoint.img analysis):** the checkpoint captures the
**OCI spec**, which references token files as **bind-mount paths**, not contents. After
restore-through-lifecycle repoints those mounts at **Pod B's** kubelet-owned source dirs (see the
mapping table in `restore-through-lifecycle.md`), **the files on disk are already correct and
fresh.** The only stale copies are the ones the agent **already read into RAM before freeze.** So
rehydration of K1/K2 is overwhelmingly about **cache invalidation**, not file plumbing — force the
consumers to re-read what is already correct on disk.

### 2.2 Connections / network identity

| # | Connection | Established by | Survives raw runsc restore? | Thaw action |
|---|---|---|---|---|
| N1 | **NATS publisher** (`NatsPublisher::connect`, `run.rs:928`) | coop at boot; one async-nats client | FD restored but TCP 4-tuple dead after IP change (ka-0000004928) | **force reconnect** |
| N2 | **NATS relay** pub + sub (`run.rs:962`) — drives coopmux discovery + `announce` | coop at boot | same | **force reconnect + re-announce** |
| N3 | **NATS forwarder** (`run.rs:1012`) — text/usage events to bridge/beads3d | coop at boot | same | **force reconnect** |
| N4 | **Inbox JetStream consumer** (`run.rs`, GT_ROLE inbox) | coop at boot | same | **force reconnect** (durable consumer resumes from last ack) |
| N5 | **coop nats-bridge sidecar** (`nats_bridge.go`) — loopback WS→AVA proxy carrying the cookie | `gb agent start` (Go side) when an AVA cookie exists | sidecar process is part of the checkpointed tree; its upstream TCP is dead and its cookie may be stale | **restart the bridge** so coop's `ws://127.0.0.1:<port>/` is re-proxied with a current cookie |
| N6 | **Heartbeat loop** (`agentHeartbeatLoop`, 5-min `PreToolUse` hook → presence tracker) | `gb agent start` Go side | timer thread restored but next POST uses stale beads endpoint/token | **fire one immediate heartbeat post-thaw**, then resume cadence |
| N7 | **DNS / `KUBERNETES_SERVICE_*` env** | baked at create | env stale; resolver cache stale | rely on B's repointed `/etc/resolv.conf` (kubelet-provisioned) + re-resolve on reconnect; do **not** trust cached `KUBERNETES_SERVICE_HOST` env — prefer in-cluster config that re-reads |

**Why async-nats auto-reconnect is *not* sufficient.** async-nats does reconnect internally, but it
reconnects to the **same URL with the token captured at `connect()` time** (`build_connect_options`,
`nats.rs:296`). After a thaw the captured token may be stale (K3) and, more importantly, the
client's reconnect is driven off the *existing* socket's error — which on a restored-but-dead FD may
not surface until the first write, and even then reconnects with the old credentials. We need an
**explicit, deterministic teardown + fresh `connect()`** with a freshly-resolved token, not a
reliance on the library noticing a broken pipe.

---

## 3. Architecture: where the thaw hook lives and how it fires

```
                 ┌────────────────────── controller (thaw verb, ka-0000003405 §4) ──────────────────────┐
 freeze ──▶ snapshot ──▶ create placeholder Pod B ──▶ wait Ready ──▶ repoint+runsc restore ──▶ process live │
                                                                                          │                 │
                                                                                          ▼                 │
                                                              ╔══════════ THAW HOOK (this design) ════════╗ │
                                                              ║  Stage 0  detect-thaw (epoch bump)         ║ │
                                                              ║  Stage 1  poison in-RAM creds (C1/C2)      ║ │
                                                              ║  Stage 2  re-read token files (K1,K2,K3)   ║ │
                                                              ║  Stage 3  reconnect NATS x4 + bridge (C3)  ║ │
                                                              ║  Stage 4  re-announce presence (C4)        ║ │
                                                              ║  Stage 5  release agent main loop / report ║ │
                                                              ╚════════════════════════════════════════════╝ │
                                                                                          │                 │
                                                                                          ▼                 │
                                                                       agent resumes issuing work           │
                 └──────────────────────────────────────────────────────────────────────────────────────────┘
```

### 3.1 The trigger: a thaw epoch, not a process restart

The defining property of thaw is **the process never restarts** — `main()` does not run again, so we
cannot lean on boot-time init. We need an explicit signal *into* the running process. Three
mechanisms, in preference order:

- **(A) Restore-time marker file + inotify (preferred, robust).** The controller's thaw verb, right
  after `runsc restore` succeeds and *before* unpausing/releasing the agent, writes a small JSON
  marker to a path the agent watches (e.g. `$COOP_STATE/thaw-epoch.json` containing
  `{"epoch": N, "pod_uid": "<B-uid>", "node": "<B-node>", "ts": ...}`). coop and the `gb` Go
  supervisor each run a lightweight inotify watcher (or a 1 s poll fallback — gVisor inotify works
  but poll is a safe floor) that, on epoch increment, runs the rehydration sequence. This is the
  **SnapStart afterRestore analogue** and is fully under controller control (it can refuse to release
  the agent until the marker is consumed).

- **(B) SIGUSR1/SIGCONT convention.** The controller signals the agent supervisor; the handler runs
  rehydration. Simpler, but a signal carries no payload (epoch/pod-uid), and signal delivery across a
  runsc restore needs verification. Use only as a nudge on top of (A).

- **(C) Self-detection of identity drift.** The agent periodically compares its cached pod-uid /
  node against `/etc/hostname` + the SA-token's `kubernetes.io/serviceaccount/namespace` mtime; on
  mismatch, self-trigger. This is a **safety net**, not the primary path — it's racy and may fire
  late. Keep it as a backstop so an un-marked thaw still eventually rehydrates rather than silently
  rotting.

**Decision:** implement **(A) as primary, (C) as backstop.** (A) gives the controller a hard gate
(don't release the agent until rehydration acks); (C) guarantees eventual correctness even if the
marker write is skipped. (B) is optional polish.

### 3.2 Who runs each stage

The agent pod is two cooperating processes (per `agent_start.go`): the **`gb` Go supervisor** (owns
heartbeat N6, escalation watcher, account/cred resolution K3/K4, the nats-bridge sidecar N5) and
**coop** (owns the four NATS transports N1–N4 and the agent session). The thaw hook therefore has
**two consumers** of the same epoch marker:

- **`gb` supervisor**: re-runs `resolve-auth.sh` cred resolution if needed (K4), restarts the
  nats-bridge with a current cookie (N5), fires an immediate heartbeat (N6), re-reads beads
  endpoint/token (K3).
- **coop**: tears down and re-`connect()`s N1–N4 with a freshly-resolved token, re-announces via the
  relay (N2 → C4). This is a new coop control-plane command (e.g. an internal `rehydrate` RPC on the
  existing coop control socket, or reacting to the marker directly).

Both ack completion (write `thaw-epoch.json.done` / respond on the control socket); the controller's
thaw verb waits for both acks (or the deadline) before declaring the agent live.

---

## 4. Stage-by-stage design

### Stage 0 — detect-thaw (epoch bump)

Controller writes the marker post-restore. Each watcher reads `epoch`; if `> last_seen_epoch`, it
latches the new epoch and proceeds. **Idempotency:** rehydration must be safe to run twice (a poll +
inotify double-fire, or a backstop firing after the primary). Every stage below is written to be
idempotent (re-reading a token file twice is harmless; reconnecting an already-fresh NATS client just
costs one extra connect).

### Stage 1 — poison in-RAM credential caches (C1, C2)

This is the stage that makes thaw *correct* rather than *lucky*. We must guarantee no Pod-A token is
reused:

- **AWS SDK (K1):** invalidate the in-process credential provider cache. For the Go controller path,
  reconstruct the `aws.Config` / credential provider so the next call re-runs
  `AssumeRoleWithWebIdentity` reading the (fresh) token file. For any agent-side AWS usage, the SDK's
  web-identity provider re-reads `AWS_WEB_IDENTITY_TOKEN_FILE` on refresh — but its cached STS creds
  may not have expired yet, so we **force-expire** them (set the provider's expiry to now / drop the
  cached `aws.Credentials`). *Do not* trust "the token will expire on its own" — that's the
  identity-confusion window.
- **beads/k8s bearer (K2, K3):** any long-lived client holding a cached bearer (the heartbeat poster,
  `kd` is per-invocation so it's fine) re-reads its token source. In-cluster clients should use the
  token file path, not a value snapshotted at boot.
- **Mark the freeze line:** record `frozen_at`; any credential whose on-disk mtime predates
  `frozen_at` *and* whose in-RAM copy we hold gets dropped.

### Stage 2 — re-read token files (K1, K2, K3, conditionally K4/K5)

Mechanically simple because restore-through-lifecycle already repointed the mounts at Pod B's
kubelet-owned dirs. This stage just forces the **consumers** to read what's already correct:

- Re-`open()`+read `AWS_WEB_IDENTITY_TOKEN_FILE` and the SA token path; confirm non-empty and parse
  the JWT `exp` to sanity-check freshness (reject if still A's token — compare `sub`/`aud` or issue
  time vs `frozen_at`).
- K4 (account creds): stat `$CREDS_PVC/.credentials.json`; re-run `_resolve_account_auth` **only** if
  missing or a rotation marker bead changed. Avoid a needless coopmux round-trip on every thaw.
- K5 (git): re-run `writeGitCredentials` best-effort.

### Stage 3 — reconnect NATS + restart bridge (C3)

The core connection work. For coop's four transports:

1. **Resolve a current NATS endpoint + token** (`config.nats_token_resolved()` re-evaluated; if the
   AVA path is in use, the endpoint is the loopback bridge URL — which must be re-pointed at the
   restarted bridge, see below).
2. **Cancel** each transport's task via its existing `CancellationToken`/`shutdown` (these already
   exist — `publisher.run(store, shutdown)`), which drops the dead async-nats client cleanly.
3. **Re-`connect()`** each (`NatsPublisher::connect`, `NatsRelay::connect`, `NatsForwarder::connect`,
   inbox consumer) with the fresh token, and re-spawn the run tasks. The durable JetStream inbox
   consumer (N4) resumes from its last ack — no message loss across the freeze.
4. **nats-bridge (N5):** the Go supervisor restarts the loopback bridge (`startNATSBridge`) with the
   current AVA cookie; coop's reconnect in step 3 then dials the **new** loopback port. Sequencing:
   bridge-restart must complete **before** coop reconnects when AVA is in play. The epoch marker can
   carry the new loopback URL, or coop re-reads it from a known path.

**Reconnect tuning:** on the fresh `connect()`, prefer bounded retry (a few attempts with backoff,
capped well under the Stage-5 deadline) so a transiently-not-yet-routable network on a brand-new pod
doesn't fail the whole thaw on the first try.

> **IMPLEMENTED (bug ka-0000001492, fix dir C).** The first prod thaw whose restore reached `rc==0`
> on both root + sub fail-closed here: the restored runsc sentry's first NATS reconnect resolved the
> bare service name `gardenio-nats` and got `EAI_AGAIN` ("Temporary failure in name resolution") — a
> *transient* cold-netns DNS miss (the fresh CNI netns had no working egress to kube-dns yet), **not**
> a config/`EAI_NONAME` defect (`restore-into-b.py` repoints `/etc/resolv.conf`+`/etc/hosts` to Pod B's
> kubelet files, and the boot-wedge DNSConfig appends the release-ns search domain — so the bare name
> *does* resolve once kube-dns is reachable). The single-shot `connect()` burned the whole 60 s deadline
> on one failed dial. The bounded retry above was **prescribed but never wired** — now it is:
> `NatsSupervisor::reconnect_all` retries the *required* transports (publisher N1, relay N2) under a
> deadline anchored at handler entry (`RECONNECT_RETRY_FRACTION = 0.85` of `marker.deadline()`, capped
> backoff 100 ms→1 s) in `nats_supervisor.rs`; best-effort transports (forward N3, inbox N4) stay
> single-shot. The **boot** path (`connect_all`) passes no deadline → single-shot, unchanged. This
> keeps boot+thaw on **one** DNS mechanism (k8s-native DNSConfig + bare names, decision ka-0000002758):
> no URL rewriting — thaw just waits for kube-dns reachability under the gate.

> **HARDENED (bug ka-0000001492, fix dir A — deterministic pre-resolve).** Re-drive r22 (prod
> 2026.159.18) proved the bounded retry above was *necessary but not sufficient*: coop *did* eventually
> ack epoch 1 `ok:true`, but **+95 s past** the 60 s deadline. Root cause of the slowness: the configured
> endpoint is a **bare single-label** name (`nats://gardenio-nats:4222`). On the cold post-restore CNI
> netns, with k8s `ndots:5` + the appended release-ns search domain, **each** `getaddrinfo("gardenio-nats")`
> walks the *entire* search list — and while the netns has no egress to kube-dns yet, *every* query in
> that walk blocks to its full per-query timeout before the call returns `EAI_AGAIN`. So a *single*
> `connect()` attempt can consume tens of seconds, and the retry loop barely iterates inside the budget —
> the ack lands, but far too late. The fix is to **take DNS off the cold path entirely**: at boot, while
> DNS is warm, `NatsSupervisor::prewarm_dns()` resolves the bare host **once** and caches the IP in RAM
> (`cached_ip: (host, ip)`). A gVisor freeze snapshots that heap value, so on thaw `spawn_generation`
> rewrites the dial URL's host to the cached **IP literal** (`rewrite_host_to_ip`) and `connect()` does
> **zero DNS** — it lands the instant the netns gains egress to the NATS ClusterIP. The ClusterIP VIP is
> stable for the Service's lifetime, so the cached IP is exactly what DNS would have returned. This stays
> within decision ka-0000002758: the controller still emits the bare name + DNSConfig (no controller-side
> URL rewrite); coop does the IP substitution **in-process**, only on the thaw path, and only when the
> effective endpoint still targets the pre-resolved host (never the AVA loopback bridge URL). The
> entry-anchored retry remains as the fallback for the rare case where prewarm itself could not resolve
> (e.g. boot DNS not yet ready) — bare name under deadline. Prewarm is gated on `thaw_state_dir` so
> non-gVisor pods pay nothing.

> **HARDENED again (bug ka-0000001492, fix dir A cont'd — short per-attempt connect timeout).**
> Canary eg8 (prod 2026.160.7) was the first drive where Pod B stayed alive the *full* 60 s window
> (reaper-exemption proven) — yet coop *still* failed to ack within deadline. DNS was already off the
> cold path (the boot log shows `pre-resolved NATS host … ip=…`), so the residual failure is **route
> reachability**, not name resolution: on a freshly-grafted Pod B netns, kube-proxy may not have
> programmed the DNAT/iptables rule for the NATS ClusterIP for several seconds, so a TCP SYN to that
> VIP is black-holed. async-nats's default `connection_timeout` is **5 s** and caps the *whole*
> handshake, so each retry attempt blocks the full 5 s before erroring — that defeats the 100 ms→1 s
> backoff, leaving only ~8 probes inside the 0.85×60 s window. The fix: on the **thaw** path only,
> `spawn_generation` caps each required-transport `connect()` attempt's handshake at a short
> `THAW_CONNECT_TIMEOUT` (default 2 s, override `COOP_THAW_NATS_CONNECT_TIMEOUT_MS`) via async-nats's
> `connection_timeout`, so a black-holed SYN fails fast and the bounded retry re-probes ~25×, landing
> the instant kube-proxy programs the route. The **boot** path passes `None` → keeps the 5 s default
> (network is warm; boot semantics unchanged). Best-effort transports (forward N3, inbox N4) stay
> single-shot at the 5 s default.

### Stage 4 — re-announce presence (C4)

- coop's **relay** (N2) emits its `announce` message on (re)connect — re-establishes coopmux
  discovery. Confirm the relay's connect path sends announce eagerly (if it only announces on a timer,
  add an eager announce on reconnect).
- The Go supervisor fires **one immediate heartbeat** (`emitAgentHook(..., "PreToolUse", ...)`)
  rather than waiting up to 5 min for the next tick (N6) — this re-arms the presence tracker (15-min
  reaper threshold) and re-registers with the local registry / mux if needed
  (`localreg.Register`, `muxClient.Register`). The heartbeat is an HTTP POST to the cross-ns beads
  daemon (a bare service name), so it shares the same cold-netns DNS exposure as coop's NATS
  reconnect; it is now retried under the handler's marker deadline (`retryUntilDeadline` in
  `agent_start_thaw.go`, same capped backoff) so a transient `EAI_AGAIN` is absorbed rather than
  fail-closing the thaw (bug ka-0000001492). A ctx with no deadline (direct unit calls) runs
  single-shot.
- Optionally squawk a one-line `agent thawed (epoch N, node X)` so the operator sees the transition.

### Stage 5 — release the agent main loop / report (C5)

- Both consumers write their `.done` ack (with per-stage status). The controller's thaw verb collects
  acks; on **all required stages green within deadline**, it unpauses/releases the agent (if it held
  it paused) and marks the agent live.
- On **deadline exceeded or a required stage failing**, fail-closed: the controller does **not** mark
  the agent live; it logs the failing stage, optionally retries the failing stage once, then either
  aborts the thaw (discard Pod B, leave the snapshot intact for a retry) or fails the pod for
  operator attention. The agent must never resume issuing authenticated work in a half-rehydrated
  state.

---

## 5. Deadlines, ordering, and failure semantics

**Ordering is load-bearing** (each arrow is a hard dependency):

```text
Stage 0 ─▶ Stage 1 ─▶ Stage 2 ─▶ [bridge restart (N5)] ─▶ Stage 3 (NATS reconnect) ─▶ Stage 4 ─▶ Stage 5
                                   └─ only when AVA cookie path is active ─┘
```

- **Why creds before connections:** NATS auth (and the AVA bridge cookie) consume the tokens from
  Stages 1–2. Reconnecting before re-reading would reconnect with a poisoned token.
- **Deadline:** propose **10 s** total budget. Rationale: thaw itself is sub-300 ms; STS
  AssumeRoleWithWebIdentity is typically <1 s; NATS reconnect <1 s; the budget is dominated by retry
  headroom on a freshly-networked pod. 10 s is comfortably under the presence reaper (15 min) and the
  harness SIGTERM window. Make it a tunable (`THAW_REHYDRATE_DEADLINE`).
- **Required vs best-effort:** **required** = Stage 1 (poison), Stage 2 for K1+K2 (the identity
  tokens), Stage 3 for N1+N2 (publisher + relay, since relay drives presence), Stage 4 heartbeat.
  **best-effort** = K4/K5, N3 (forwarder), N4 (inbox — durable, self-heals), squawk. A best-effort
  failure logs but does not fail-close.
- **Idempotency + at-least-once:** primary (marker) + backstop (drift self-detect) means rehydration
  may run more than once. All stages are idempotent; a second run is a cheap no-op when nothing
  changed.

**Failure-mode matrix:**

| Failure | Detection | Response |
|---|---|---|
| Token file still holds A's identity (mount not repointed) | JWT `aud`/`sub`/`iat` check in Stage 2 | fail-close — restore-through-lifecycle precondition violated; abort thaw |
| STS refuses refreshed web-identity | first AWS call / explicit warm STS probe in Stage 2 | retry once; then fail-close (the agent cannot act as itself) |
| NATS reconnect fails within deadline | Stage 3 connect error after bounded retry | required for N1/N2 → fail-close; best-effort for N3/N4 → log + continue |
| Bridge cookie stale (AVA) | bridge upstream upgrade 401/403 | re-read cookie from credstore; if still bad → fail-close (no NATS path) |
| Marker never written (controller bug / manual restore) | drift backstop (C) fires late | rehydrate on detection; emit a warning that the primary path was skipped |
| Rehydration runs twice | epoch latch + idempotent stages | second run is a no-op |

---

## 6. Verification & E2E plan

Built on the existing harness (`restore-through-lifecycle.sh` + `restore-into-b.py`), which already
proves the netns/volume/token *plumbing*. The rehydration tests assert the *correctness contract*:

1. **Cred re-mint (C1/C2) — the headline test.** Freeze an agent **holding a near-expiry STS cred**
   in RAM (seed by reading the token early + sleeping). Thaw into Pod B. Assert: (a) the agent's next
   AWS call **succeeds** and (b) it is observably made under **Pod B's** SA — verify via CloudTrail /
   the assumed-role session name carrying B's pod identity, *not* A's. This is the test that catches
   the identity-confusion hazard.
2. **Token-freshness gate (Stage 2).** Corrupt/blank Pod B's token file before release; assert the
   thaw **fails closed** (agent not marked live) rather than running with A's RAM token.
3. **NATS reconnect (C3).** Freeze with an active NATS publisher; thaw onto a **different node/IP**;
   assert a message published *after* thaw is received by a subscriber, and that the pre-thaw client
   FD was torn down (no write to the dead 4-tuple — assert no error spew / no silent drop). Repeat per
   transport N1–N4; for N4 assert the durable consumer resumes from last ack (no gap, no dup-storm).
4. **Presence re-announce (C4).** Assert the agent re-appears in `gb agent roster` / coopmux within
   the deadline after thaw, and the presence-tracker `last_seen` advances (heartbeat fired
   immediately, not on the 5-min tick).
5. **Idempotency.** Fire the epoch marker twice; assert exactly-once *effect* (one set of live
   connections, no leaked clients) and no error.
6. **Deadline.** Inject a NATS endpoint that never accepts; assert the thaw fails-closed at the
   deadline with the failing stage logged, and the snapshot is left intact for retry.
7. **AVA path (N5).** With the cookie/bridge active, assert bridge restart precedes coop reconnect
   and a stale cookie fails-closed.

Wrap all `runsc`/restore calls in `timeout` with post-state assertions (epic constraint
ka-0000006959: runsc C/R can hang on process exit). Run on the `gvisor-poc` pool / ns.

### 6.1 Built (ka-0000005803) — where each test landed

The §6 plan is implemented across two complementary, CI-verifiable suites that split the contract
by what each half can *truly observe*:

- **Connection half — live-broker integration** (`coop/tests/specs/tests/rehydration.rs`): drives the
  real `NatsSupervisor` + `RehydrateHandler` against a spawned `nats-server`, SKIP-clean when the
  broker is absent (mirrors `smoke.rs`). Covers **#3 NATS reconnect (C3)** — publisher+relay
  reconnect onto a *different broker* (the node/IP change) and a post-thaw event is received there;
  **#4 presence re-announce (C4)** — the relay emits an eager `online` announce on every reconnect,
  well under the 30 s heartbeat; **#5 idempotency** — two epoch bumps = exactly-once effect, one OK
  ack each, no leaked generation; **#6 deadline** — a dead required endpoint fail-closes
  (`Outcome.ok == false`) within budget. Runs in `make check` / `make ci` (`cargo test --all`).
- **Credential/identity half — substrate harness** (`gardenio/tests/e2e/scripts/test-gvisor-rehydration.sh`):
  asserts **#1 cred re-mint (C1/C2)** by exercising the exact freshness signals the agent's real gate
  keys on (`rehydrate::freshness_gate`) over crafted Pod-A vs Pod-B JWTs — Pod-A `iat`/`pod_uid`
  fail-closed, Pod-B passes; **#2 token-freshness fail-close** on blank/expired/wrong-aud/indeterminate;
  and **#4/#5/#6** via the thaw-epoch marker/ack *release condition* (the `WaitForAcks` semantics) on
  disk. The contract-shape assertions need only bash + a base64 codec and run in every shard; the
  opt-in (`GVISOR_E2E=1`) section adds the runsc-lifecycle marker/ack round-trip on the substrate and
  a CloudTrail role-session-name probe scaffold for the live C1 assertion. Wired into `run-shard.sh`
  and a dedicated `gardenio-gvisor-rehydration` RWX dispatch (`.rwx/e2e.yml`).

The Stage-0/1/2 gate logic (`internal/thawepoch` gate+watcher, `rehydrate::freshness_gate`,
`agent_start_thaw` heartbeat) remain covered by their in-tree unit tests; the suites above assert
the same contract *end-to-end* through the real broker / on-disk protocol rather than via fakes.
(The AVA bridge-ordering case #7 went with the nats-bridge in gd-bK5sSblLz9 / gd-Nza6by1LUT.)

---

## 7. Implementation surfaces (where the code lands)

No code in this MR — but the design names its homes so the build bead can be scoped:

- **Controller thaw verb** — `gardenio/controller/internal/podmanager` (the natural home named by
  `restore-through-lifecycle.md` §4): writes the epoch marker post-restore, waits on rehydration
  acks, enforces the deadline + fail-close.
- **Go supervisor hook** — `gardenio/controller/cmd/gb/` (alongside `agent_start.go`,
  `agent_start_creds.go`, `nats_bridge.go`): marker watcher; cred re-resolution
  (`resolve-auth.sh` re-invoke); nats-bridge restart; immediate heartbeat; AWS provider cache
  invalidation.
- **coop rehydrate path** — `coop/crates/cli/src/run.rs` + `transport/nats*.rs`: a control-plane
  `rehydrate` action that cancels + reconnects N1–N4 with a fresh token and eager-announces. Factor
  the four `connect()` call sites (currently inline in `run.rs:920–1040`) behind a small
  `reconnect_all(token)` so thaw and boot share one path.
- **Entrypoint** — `gardenio/images/agent/` (`entrypoint.sh`, `resolve-auth.sh`): make cred resolution
  re-runnable (sourceable function already is) and ensure token reads are not cached in shell env in a
  way that survives thaw.

### Suggested build-bead breakdown (children of ka-0000006959)

1. **Thaw-epoch signal**: controller marker write + agent-side watcher (inotify + poll backstop) +
   ack protocol + deadline gate. *(blocks the rest)*
2. **Credential re-mint**: Stage 1+2 — AWS SDK cache invalidation, token-file re-read, JWT freshness
   gate, beads/bearer re-read. *(blocked by 1)*
3. **NATS reconnect**: coop `reconnect_all` + eager announce + nats-bridge restart sequencing.
   *(blocked by 1; coordinates with 2 for the token)*
4. **E2E rehydration suite**: §6 tests on the gvisor-poc harness. *(blocked by 2, 3)*

---

## 8. Open questions for the build phase

- **coop control channel for `rehydrate`:** reuse the existing coop control socket / HTTP, or react to
  the marker file directly in a coop watcher task? (Leaning: a coop watcher task is simplest and keeps
  coop self-contained; the Go side and coop both watch the same marker.)
- **AWS SDK cache invalidation idiom:** cleanest Go idiom to force web-identity STS refresh without
  reconstructing the whole `aws.Config` (force-expire the cached `aws.Credentials` vs. swap the
  provider). Confirm against the SDK version in `go.mod`.
- **Does async-nats expose a clean drop?** Confirm cancelling the run task + dropping the `Client`
  fully closes the dead socket (it should via `Drop`), so we don't leak the A-era FD.
- **Marker vs signal for the eager heartbeat:** is `emitAgentHook` safe to call re-entrantly from the
  watcher while the 5-min loop is also live? (Make the heartbeat poster mutex-guarded.)
- **Multi-thaw / re-freeze:** epoch is monotonic; confirm the snapshot metadata carries the last epoch
  so a re-frozen-then-thawed agent doesn't replay an old epoch.

---

## 9. Verdict

The restore-through-lifecycle work delivers a thawed process living in a correct, kubelet-owned
sandbox — but living **in the past**. This design closes the correctness boundary: a **SnapStart-style
post-restore hook**, triggered by a controller-written **thaw epoch**, that **poisons every in-RAM
Pod-A credential, re-mints identity from kubelet-fresh on-disk tokens, tears down and reconnects all
four NATS transports + the AVA bridge, re-announces presence, and fails-closed under a 10 s
deadline.** The expensive, security-sensitive parts (token minting, netns, volumes) are already done
by kubelet because Pod B is a real pod; rehydration is overwhelmingly **cache invalidation +
deterministic reconnect**, not new plumbing. The §6 E2E suite turns each clause of the contract
(C1–C5) into a pass/fail assertion on the existing gvisor-poc harness.
