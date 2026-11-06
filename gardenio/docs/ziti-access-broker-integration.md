# Gardenio ↔ ziti-access-broker integration (Phase 2)

Gardenio-side seam of the network-fabric **ziti-access-broker** contract. This
is the consumer half of `network-fabric-deployment`'s
`docs/gardenio-ziti-access-broker.md` — it lets an agent pod obtain
**task-scoped OpenZiti overlay access** (a private bind/dial to an in-cluster
service exposed only over the Ziti overlay, no public ingress/DNS) without ever
holding a durable overlay credential.

Tracking epic: `ka-0000000990`. Broker contract verified against
`network-fabric-deployment` branch `agent/ziti-access-broker-real-runtime`
(MR !2).

> **Status: prototype, default-OFF.** Nothing in this doc activates unless an
> operator sets `agents.zitiAccess.enabled=true` (chart) / `ZITI_ACCESS_BROKER_URL`
> (controller) **and** a project/role/agent bead opts in. A chart with the
> feature off renders byte-identically to before.

## The one idea that drives the whole design

**The lease is minted in-pod, by the agent's own ServiceAccount; the controller
only revokes.**

The broker authorizes the *subject of the identity proof*
(`system:serviceaccount:<ns>:<sa>`) against a fabric-owned allow-list. Fabric
allow-lists the **agent pod's** service account. So the `POST /lease` call must
present the **pod's own** projected token — if the controller minted the lease,
the proof subject would be the *controller's* SA and the grant would be
mis-attributed (the classic confused-deputy). Hence:

| Step | Who | Why |
|---|---|---|
| Mint lease (`POST /lease`) | **in-pod init container** | proof subject must be the pod SA |
| Enroll JWT + run tunneler | **in-pod sidecar** | overlay identity is pod-local, dies with the sandbox |
| Revoke (`DELETE /lease?task_id=`) | **controller** | controller is a configured revoker; releases the lease on pod exit |

The broker's audience pin is the teeth of the defense: the projected token is
minted for audience `gardenio-ziti-broker`. A pod's IRSA `sts.amazonaws.com`
token (which it already carries) is **rejected** by the broker because the
audience does not match — so a pod cannot repurpose an existing credential to
mint a lease.

## What got built (this MR)

All in `gardenio/controller`:

### Pod construction — `internal/podmanager/ziti_access.go`
When `AgentPodSpec.ZitiAccess != nil`, the pod builders add (and **only** then):

1. **Projected broker-token volume** (`ziti-broker-token`) — a
   `serviceAccountToken` projection with `audience: gardenio-ziti-broker`,
   `expirationSeconds: 600`, mounted read-only at `/var/run/secrets/ziti-broker`.
   tmpfs-backed; never on node disk.
2. **Memory-backed identity emptyDir** (`ziti-identity`) at `/var/run/ziti` —
   shared between the init container and the sidecar. `medium: Memory` so the
   one-time JWT and the enrolled identity (with its private key) never touch
   node disk.
3. **`ziti-lease` plain init container** — runs the agent image (has `curl`+`jq`,
   already pulled), `POST`s `/lease` with the pod's own broker token, and writes
   the returned `enrollment_jwt` to `/var/run/ziti/enroll.jwt` (0600) plus a
   **JWT-stripped** `lease.json` for debugging. Fail-closed unless
   `ziti_optional=true`. **Never logs the token or the JWT.**
4. **`ziti-tunnel` native sidecar** (`restartPolicy: Always`) — runs the
   configured `ziti-edge-tunnel` image: `enroll --jwt … --identity …`, shreds
   the one-time JWT, then `run --identity-dir …`. Needs `NET_ADMIN` +
   `/dev/net/tun`.
5. **Agent-container env** — `ZITI_ACCESS_BROKER_URL`,
   `ZITI_ACCESS_BROKER_TOKEN_FILE`, `ZITI_ACCESS_SERVICE_ID`,
   `ZITI_IDENTITY_DIR` — so the workload can discover the broker (e.g. to
   self-revoke) and the pod-local identity. Read-only mounts of the token +
   identity dir.

### Revoke-on-exit — `cmd/controller/ziti_revoke.go`
On `AgentDone`/`AgentKill`/`AgentStop` (and the stuck-restart + closed-bead
paths), the controller calls `DELETE /lease?task_id=<agentBeadID>` with its own
projected broker token. **`task_id` is the durable correlation key** — the
controller revokes by the agent bead ID it already owns, so it does **not** need
to persist the opaque server-minted `lease_id`. Best-effort: a failure is logged
but never blocks teardown; the lease TTL is the hard backstop. Runs on a
detached context so a controller roll mid-teardown still releases the lease.

### Opt-in plumbing — `cmd/controller/podspec.go` (`applyZitiAccess`)
Resolves Ziti settings across **global < project < role < agent** precedence
(mirroring the docker/postgres toggles), combines with controller config, and
populates `spec.ZitiAccess`. Fail-safe: if a bead opts in but no service id is
resolved or no tunneler image is configured, it logs and leaves the feature OFF
rather than emitting a broken pod.

### Config + chart
- Controller env: `ZITI_ACCESS_BROKER_URL`, `ZITI_ACCESS_TOKEN_AUDIENCE`,
  `ZITI_TUNNELER_IMAGE`, `ZITI_ACCESS_DEFAULT_TTL`, `ZITI_ACCESS_REVOKE_ON_EXIT`,
  `ZITI_ACCESS_CONTROLLER_TOKEN_FILE`.
- Helm: `agents.zitiAccess.{enabled,brokerURL,tokenAudience,tunnelerImage,defaultTTL,revokeOnExit}`,
  plus the controller's own projected broker-token volume when `revokeOnExit`.

## Bead fields (opt-in surface)

Set on a `type=project`, `type=role`, or agent bead (agent metadata wins):

| Field | Meaning |
|---|---|
| `ziti_access` | `"true"` to opt in; `"false"` on an agent is a kill-switch |
| `ziti_service_id` | resolved OpenZiti **service id** the pod leases (required) |
| `ziti_bind` | `"true"` for a Bind (server) lease; default Dial (client) |
| `ziti_ttl` | requested lease duration, e.g. `"15m"` (broker clamps) |
| `ziti_optional` | `"true"` makes a failed mint non-fatal (pod still starts) |

> **Service id vs. name.** Fabric's policy ConfigMap is keyed by stable
> `service_name`, but the broker authorizes the lease request on the resolved
> Ziti **service id**. So the bead must carry the resolved `ziti_service_id`
> today. A future ergonomic improvement is to let the pod request by
> `service_name` and have the broker resolve it (it already resolves names→ids
> when loading the policy) — tracked as a follow-up.

## Operator runbook

1. **Fabric side** (network-fabric-deployment): add the agent pod's
   ServiceAccount to the broker policy ConfigMap with the service name(s) +
   direction(s) it may lease; add the gardenio controller SA to
   `revoker_subjects`.
2. **Controller side** (chart values):
   ```yaml
   agents:
     zitiAccess:
       enabled: true
       brokerURL: http://ziti-access-broker.ziti-access-broker.svc.cluster.local
       tunnelerImage: openziti/ziti-edge-tunnel:<pinned>
       defaultTTL: "15m"
       revokeOnExit: true
   ```
3. **Per-consumer opt-in**: set `ziti_access=true` + `ziti_service_id=<id>` on
   the project or role bead for the agents that need overlay access.
<a name="namespace-psa"></a>
4. **Namespace PSA**: the tunnel sidecar needs `NET_ADMIN` **and** a
   `/dev/net/tun` hostPath volume. PSA `baseline` rejects **both** — it permits
   only the default capability set plus `NET_BIND_SERVICE`, and bars `hostPath`
   outright — and `restricted` rejects them too. So the fleet namespace must be
   PSA **`privileged`** to admit this pod shape; it will **not** admit under
   `baseline` or `restricted`. The controller enforces this before pod create
   (`checkZitiAdmittable`, `internal/podmanager/ziti_access.go`): an opted-in
   pod in a `baseline`/`restricted` namespace fails fast with an actionable
   error naming the namespace and the fix, instead of a cryptic admission-
   webhook denial. Set `ziti_optional=true` on the bead to degrade gracefully
   instead — the controller strips the Ziti wiring so the pod still starts,
   without overlay access. The non-privileged-namespace-safe alternative is the
   SDK/proxy enrollment shape (workload links the Ziti SDK, no tun device, no
   `NET_ADMIN`) — see follow-up 1 below.

> ### ⚠️ Two independent gates: PSA admission ≠ broker authorization
>
> Steps 1 and 4 are **two separate gates on two different subjects**, and moving
> a pod to satisfy one can silently break the other. **Verified live against
> `prod-gardenio-eks` on 2026-08-27** (bead `ka-0000003101`):
>
> | Gate | Keyed on | `gardenio-system` | `gardenio-gvisor` |
> |---|---|---|---|
> | **PSA admission** (TUN sidecar: `NET_ADMIN` + `/dev/net/tun`) | namespace PSA label | ❌ `baseline` — rejects TUN | ✅ `privileged` — admits TUN |
> | **Broker authorization** (`POST /lease`) | `system:serviceaccount:<ns>:<sa>` | ✅ **HTTP 200** (lease minted) | ❌ **HTTP 403** "not authorized" |
>
> The broker allow-list is **namespace-qualified** (step 1). Today the fabric
> `ziti-access-broker-consumer-policy` grants exactly one k8s subject —
> `system:serviceaccount:gardenio-system:gardenio-agent` (Dial+Bind on
> `gardenio-prod-beads-viewer`). Routing the opted-in pod to a `privileged`
> namespace to satisfy PSA (e.g. the gVisor namespace, via
> `routeGVisorNamespace`) **changes its subject to
> `…:gardenio-gvisor:gardenio-agent`, which is not on the allow-list** — the pod
> admits, then its lease mint is refused with a `403`. A projected in-pod token
> for the same SA in `gardenio-system` mints a lease (200); the identical shape in
> `gardenio-gvisor` is refused (403). So the two gates are **mutually exclusive
> for the TUN shape** as currently configured, and there is **no single
> namespace** that satisfies both. Resolving it requires **one** of:
>
> 1. **Fabric-policy addition** (network-fabric-deployment): add
>    `system:serviceaccount:<privileged-ns>:gardenio-agent` to the consumer
>    policy for the namespace the TUN pod actually lands in. This is an
>    **external** change owned by the fabric team, not an in-gardenio toggle.
> 2. **SDK/proxy enrollment shape** (follow-up 1): no TUN → admits under
>    `baseline` → the pod **stays in `gardenio-system`** and keeps the
>    already-allow-listed subject. This is the only path that needs **no**
>    fabric-policy change, but the SDK/proxy sidecar is **not yet built**.
>
> `checkZitiAdmittable` only guards the PSA gate; it cannot see the fabric
> allow-list, so a subject/namespace mismatch surfaces at runtime as a `403`
> from the `ziti-lease` init container, not at admission time.

## Debugging stuck access

The three seams log distinctly so you can localize a failure without leaking
secrets:

- **`ziti-lease` init container** — `[ziti-lease]` lines. `broker http=<code>`
  tells you mint outcome: `401`=token/audience, `403`=policy (subject/service/
  direction/TTL not allowed), `400`=bad request, `502`=broker→Ziti mint failure.
  Broker-unreachable is a distinct "curl transport error" line.
- **`ziti-tunnel` sidecar** — `[ziti-tunnel]` lines: enroll success/failure
  (expired/consumed JWT, controller unreachable), then tunneler start.
- **Controller** — `ziti lease revoked on agent exit` / a warning with the
  `401/403 => not a revoker subject` hint.

## Known constraints / follow-ups (for the contract conversation)

1. **Tunneler needs a non-restricted PSA namespace** (NET_ADMIN + tun device).
   The SDK/proxy enrollment shape (workload links the Ziti SDK, no tun device)
   is the restricted-namespace-safe alternative and drops in behind the same
   lease/JWT handoff — the init container and revoke path are unchanged. It is
   also the only shape that keeps the pod in `gardenio-system` (the sole
   allow-listed SA subject) — see the two-gate warning under runbook step 4:
   the TUN shape cannot currently satisfy PSA admission and broker
   authorization in one namespace without a fabric-policy addition.
2. **Service-id-not-name** request asymmetry (see above).
3. **`/dev/net/tun` hostPath** is required for TUN mode; a cluster that runs the
   `device-plugin` style tun provisioning could replace the hostPath.
4. **Tunneler image** is operator-pinned (`agents.zitiAccess.tunnelerImage`).
   Fabric may publish an approved/CVE-tracked image to pin instead.

See also: `network-fabric-deployment/docs/gardenio-ziti-access-broker.md`
(the contract) and `…/docs/ziti-access-broker-eks-consumer-runbook.md` (the
fabric onboarding runbook).
