# C6 — NATS subject scoping and separate NATS auth

**Status:** Design ADR / scope analysis. No implementation. Operator decision needed before code starts.
**Tracks:** [ka-0000005594](https://beads-viewer.gardenio.e2e.example.com/ka-0000005594) — Epic C [ka-0000005919](https://beads-viewer.gardenio.e2e.example.com/ka-0000005919) (internet-safe daemon).
**Author:** sched-local-agent-security-epi-9elj.
**Date:** 2026-04-29.
**Predecessors (read these first):**
- `gardenio/docs/local-agent-epic-brief.md` — Epic A/B/C handoff brief.
- `gardenio/docs/research/c1-token-ttl-refresh-design.md` — companion C1 design (same template, same epic).

## TL;DR

C6's brief — "Daemon bearer token currently doubles as NATS token; holders can subscribe to arbitrary subjects. Issue separate NATS creds per agent; enforce subject ACLs so an agent only sees its own escalations/events." — is **half done already** by the existing `subjectIsolation` two-user feature in the Helm chart (infra vs. agent classes with separate tokens and a permissions ACL). The other half — *per-agent* identity rather than *per-class shared* identity — is unbuilt and forks on three mechanisms.

What is true today:

1. Daemon HTTP bearer token and NATS token are **already separate K8s Secrets** when `nats.auth.subjectIsolation.enabled=true` (`gardenio-nats-agent-creds.token` vs. the daemon's primary auth token). The brief's framing slightly understates this — the conflation is a default-config concern, not a structural one.
2. All agent pods share the **same** `NATS_AGENT_TOKEN`. Subject ACLs are class-wide: every agent can publish to `coop.events.>` (any actor namespace), and every agent can subscribe to `_INBOX.>` (any inbox).
3. Agent pods **do not subscribe** directly to hook/decision/inbox event subjects today; they read via JetStream pull consumers with `filter_subjects` (server-side filtering, **not** auth-enforced — see `coop/crates/cli/src/transport/inbox.rs:209-234`). A compromised agent could trivially widen its filter or open its own consumer with a different filter and read sibling-agent traffic.
4. Subject-scoping helpers in the daemon (`kbeads/internal/eventbus/streams.go` — `SubjectForHookEvent`, `SubjectForDecisionEvent`, `SubjectForMessageEvent`) already produce per-actor subjects (`coop.events.messages.<actor>.<eventtype>`), but the agent NATS user permissions don't constrain to those.

**Recommendation: write code only after the operator picks a mechanism (decision filed alongside this MR).** Three viable shapes are §3. Each has different operator-side ergonomics; the daemon-side change is roughly the same in all three.

## 1. What's already shipped (do not re-build)

| Capability | Where | Status |
|---|---|---|
| NATS auth disabled by default | `helm/gardenio/values.yaml:1043` (`nats.enabled: false`) | **Done.** |
| Two-user subject isolation (infra/agent) | `helm/gardenio/templates/nats/statefulset.yaml:18-56` | **Done.** Gated by `nats.auth.subjectIsolation.enabled`. |
| Agent ACL: publish `coop.events.>`, `_INBOX.>`; subscribe `_INBOX.>` only | same file:36-56 | **Done.** Class-wide; not per-agent. |
| Per-class K8s Secrets (infra-creds, agent-creds) | same file:191-200; `templates/secrets.yaml` (`gardenio-nats-infra-creds`, `gardenio-nats-agent-creds`) | **Done.** ExternalSecret-friendly. |
| Pod-spec wires `NatsAgentUserSecret` → `COOP_NATS_TOKEN` env in agent pods | `controller/cmd/controller/podspec.go:514-529` | **Done.** Falls back to `NatsToken` when `NatsAgentUserSecret` empty. |
| Daemon side: subject helpers already actor-scoped | `kbeads/internal/eventbus/streams.go:149-173` | **Done.** Subjects carry `<actor>` segment; not enforced by NATS perms. |
| Coop publishes events under `BOAT_AGENT` actor identity | `coop/crates/cli/src/transport/nats.rs:150-233` | **Done.** No token-side enforcement. |
| `subjectIsolation` rollout decision | bead `ka-RogueNatsIsolationCutover` (closed); enabled in gardenio-rwx + gardenio prod | **Done.** |

**Implication:** The cross-class ("daemon ↔ agent" separation) part of the C6 brief is already in production. The remaining work is **intra-class** — preventing one agent from impersonating another or reading another agent's events.

## 2. What's still open (the C6 surface)

### 2.1 Per-agent identity on NATS

Today every agent pod gets the same `NATS_AGENT_TOKEN`. Compromise of one agent pod's token equals compromise of every agent pod's NATS access.

Surface area:
- A mechanism to issue a *unique* NATS credential per pod at spawn time (in lock-step with how PAT issues a unique daemon token per pod via `agenttoken.go:50+`).
- Pod spec wires the per-pod NATS credential as `COOP_NATS_TOKEN` (or a JWT-style creds file) instead of the shared class token.
- Revocation path on pod-done that invalidates the per-pod credential (the daemon already has `revokeAgentToken` symmetry to lean on).
- Bootstrap path for the controller and bridges, which need infra-class access (no change — they keep using `NATS_INFRA_TOKEN`).

### 2.2 Tighter ACLs (publish-side)

Today the agent class is allowed `publish: ["coop.events.>", "_INBOX.>"]`. With per-agent identity, this should narrow to `publish: ["coop.events.<actor>.>", "_INBOX.>"]` — i.e., an agent can only publish into its own actor namespace.

Surface area:
- ACL template that interpolates the actor name into the permissions block (NATS supports `$ACCOUNT` / templated user vars; with NKEYs/JWT this is per-user permissions in the issued JWT).
- Daemon-side check that the `actor` field in inbound published events matches the authenticated NATS user (defense-in-depth; the NATS server already rejects on subject mismatch).

### 2.3 Tighter ACLs (subscribe-side)

Today the agent class is allowed `subscribe: ["_INBOX.>"]`. The `_INBOX.>` subject is NATS request-reply; per-pod scoping with NATS is automatic if the inbox prefix is unique per connection (which `nats.go` does by default per `NewInbox()`). However, **JetStream pull consumers do not use `_INBOX.>` subjects in the way `subscribe:` ACLs scope** — they go through the JetStream API on `$JS.API.>`. To fully constrain an agent to its own consumer, we need either:

- ACL: deny `$JS.API.>` for the agent class entirely, and have the daemon push events to per-agent subjects the agent subscribes to (inverts the current pull model). **High blast radius**: changes the entire event-delivery architecture.
- ACL: allow `$JS.API.CONSUMER.MSG.NEXT.<stream>.<consumer>` only where `<consumer>` is templated to the agent's actor (NATS 2.10 supports tagged subjects in user permissions).
- Auth-callout / a daemon-side proxy that mediates JetStream API calls and rewrites filters.

### 2.4 Daemon HTTP bearer ≠ NATS token (cleanup)

The brief frames the "daemon bearer token doubles as NATS token" as a live problem. Inspecting the code:

- When `subjectIsolation.enabled=true`, the daemon **does not** double up — it has its own daemon HTTP bearer (PAT or admin token) and the NATS clients use `NATS_INFRA_TOKEN` (controller, bridges, daemon SSE→NATS publisher) or `NATS_AGENT_TOKEN` (agent pods).
- When `subjectIsolation.enabled=false` and `nats.auth.token` is set (legacy single-token auth), all NATS clients share that one token, but it is **not** the daemon HTTP bearer. The conflation in the brief seems to be about the "single token of last resort" pattern, not actual token reuse.

C6 should still **explicitly document** the separation contract and add a startup-time guard in the controller: refuse to start if `NatsToken == DaemonAuthToken` (defense-in-depth against an operator pasting the same token into both env vars).

## 3. Three minimum-viable shapes for C6

### Shape A — NATS NKEYs/JWT (per-pod issued user JWT, decentralized auth)

**Scope:** Move agent class to NKEYs/JWT decentralized auth. Controller holds an account signing key. At pod spawn (alongside `provisionAgentToken`), it mints a per-pod NATS user JWT with permissions templated to the pod's actor (`coop.events.<actor>.>` publish, scoped JetStream API consumer access). Pod gets a creds file mounted via Secret. On pod-done, controller revokes the JWT via the NATS account "deny" list.

**Coverage:**
- ✅ Per-agent identity, per-agent ACLs, including JetStream API scoping via NATS 2.10 templated permissions.
- ✅ Built-in revocation primitives.
- ✅ Cleanest model long-term: each pod = unique signed identity, audit-trail-friendly.
- ❌ Adds NATS operator/account/signing-key management (a full key hierarchy: operator → account → user). Operator overhead non-trivial.
- ❌ Migration for in-cluster infra clients (controller, bridges, coopmux) — currently token-based — to also use creds-files, or run as a hybrid (mixed auth modes).
- ❌ Coop-side: switch from `nats::ConnectOptions::token()` to `creds_file()` / `nkey()`.
- **Roughly:** 600–900 LOC across controller (mint/revoke), Helm (operator/account scaffolding, ExternalSecret for signing key), coop (creds-file consumer), kbeads (infra-side creds-file consumer), plus runbook + key rotation procedure.

### Shape B — Per-pod token + dynamic NATS auth (token-list reload)

**Scope:** Keep token-based auth. Controller generates a random token per pod, writes it to a per-pod K8s Secret (`gardenio-nats-pod-<podname>`), and adds an entry to the NATS server's `authorization.users[]` list with templated permissions for that pod's actor. NATS server reload is triggered (signal or rolling restart). On pod-done, the entry is removed and the server reloaded again.

**Coverage:**
- ✅ Per-agent identity, per-agent ACLs (templated subjects per user entry).
- ✅ Coop-side unchanged (still uses `COOP_NATS_TOKEN` env var).
- ✅ No NKEYs/JWT operator scaffolding to learn.
- ❌ NATS config reload races: at fleet-scale (hundreds of pods spawning/dying per hour), `users[]` churn drives constant reloads; reloads are atomic but cause brief subscription disruption.
- ❌ Server config grows linearly with active-pod count. NATS allows ~thousands of users per server in practice, so this is ok up to ~1k concurrent agents — but ergonomics of debugging a 1k-user `users[]` block are poor.
- ❌ JetStream API scoping (§2.3) is harder under token auth than under JWT — token auth supports templated subject permissions in 2.10 but only via the user's `permissions:` block, and JetStream API subjects (`$JS.API.>`) don't carry the actor segment, so we still need the daemon-side proxy or the inverted push model. **Shape B does not solve §2.3 fully.**
- **Roughly:** 400–600 LOC in controller (config-render + reload trigger), Helm (templated config, reloadable ConfigMap pattern), no coop changes, no kbeads changes. Plus a watchdog for failed reloads.

### Shape C — Auth callout to controller (delegated authentication)

**Scope:** Use NATS 2.10 auth callout. NATS server forwards every connection's auth payload (a JWT-or-token presented by the client) to a callout service (the gardenio controller) which inspects, validates against PAT/per-pod identity, and returns a signed ephemeral user JWT with appropriate permissions. The controller becomes the single source of truth for NATS identity, mirroring its role for daemon HTTP identity.

**Coverage:**
- ✅ Per-agent identity, per-agent ACLs.
- ✅ Reuses the controller's existing PAT machinery: agent pod presents its PAT to NATS, controller validates and returns a JWT scoped to that PAT's actor. One identity provider.
- ✅ Solves §2.4 cleanly: the *only* token is PAT; NATS access is derived.
- ❌ The auth callout system is a new daemon surface area — has to be HA, fast (sub-100ms), survive restarts.
- ❌ NKEY/JWT scaffolding still needed for the auth callout's signing identity (subset of Shape A, but still more keys to manage than Shape B).
- ❌ NATS auth callout is NATS-2.10-specific and relatively new; track record at our fleet scale is unproven.
- **Roughly:** 700–1000 LOC across controller (auth callout HTTP listener, PAT-→-NATS-permissions mapping), Helm (callout endpoint config, signing key Secret), coop (use PAT as the NATS auth payload — moderate but not invasive), kbeads (no change).

## 4. Comparison

| Concern | Shape A (NKEYs/JWT) | Shape B (per-pod token + reload) | Shape C (auth callout) |
|---|---|---|---|
| Per-agent identity | ✅ | ✅ | ✅ |
| Per-agent publish ACL | ✅ templated | ✅ templated per-user | ✅ via JWT |
| Per-agent JetStream API scope (§2.3) | ✅ via templated perms | ⚠ partial | ✅ via JWT |
| Operator key hierarchy | full (operator → account → user) | none | partial (callout signing key) |
| Reload churn | none (JWT inline) | high (users[] mutations) | none |
| Single source of truth for identity | NATS account | controller config-rendering | controller PAT |
| Defense-in-depth on §2.4 | ✅ creds file ≠ HTTP bearer | ⚠ requires startup guard | ✅ PAT *is* the only auth |
| Maintenance complexity | medium (key rotation runbook needed) | medium (config-render + reload) | high (new HA service) |
| LOC (rough) | 600–900 | 400–600 | 700–1000 |

## 5. Recommendation

**Shape A (NKEYs/JWT)** is the best long-term fit. It is the canonical NATS production pattern, gives full per-agent permissions including JetStream, and aligns with how PAT already works (controller mints a credential per pod, lifecycle-bound to the pod).

The migration is non-trivial but bounded:
1. Add an account signing key to the chart (one more ExternalSecret).
2. Mint user JWTs in `agenttoken.go` alongside PAT (same call site; same lifecycle).
3. Coop switches from `token()` to `creds_file()`. Daemon and bridges keep token auth on the infra account (mixed-auth mode is supported).

**Shape B is a viable quick win** if the operator wants to land per-agent ACLs without learning the NKEYs/JWT toolchain, but it leaves §2.3 partially unsolved and has a worrying reload-churn profile at fleet scale.

**Shape C is interesting** as a consolidation of identity providers, but auth callout is the newest, least-battle-tested NATS feature and adds an HA dependency on the controller's HTTP listener for *every NATS connection*, not just bead operations. This is an availability hit I do not recommend taking now.

## 6. Out of scope (file separately if needed)

- **Per-human-operator NATS access.** Today operators don't talk to NATS directly; if/when a CLI like `gb watch` needs NATS, that's a separate identity (likely covered by the Shape A account model).
- **Cross-tenant isolation.** Multi-tenant gardenio (multiple customer namespaces sharing one NATS) is a different problem than per-pod isolation in one tenant. Defer until multi-tenant is on the roadmap.
- **NATS message-level encryption (E2EE).** TLS at the connection level is enough for the threat model in the brief.
- **Audit logging of NATS auth events.** Useful but not the C6 surface.

## 7. Decision artifacts

A `gb decision create` will be filed alongside this MR with the three options:

| id | short | label | artifact_type |
|---|---|---|---|
| `nkeys-jwt` | Shape A | NKEYs/JWT decentralized auth (recommended) | plan |
| `per-pod-token` | Shape B | Per-pod token + reload | plan |
| `auth-callout` | Shape C | Auth callout to controller | plan |
| `defer` | Defer | Keep class-wide ACL; revisit after C1/C5 land | report |

Picking any of A/B/C unlocks an implementation epic (per-pod credential issuance + ACL templating + coop client wiring + Helm scaffolding + cutover plan). The implementation epic itself will need to be re-scoped against the chosen shape.

## 8. References

- `helm/gardenio/templates/nats/statefulset.yaml` — current two-user ACL template.
- `helm/gardenio/values.yaml:1042-1071` — `nats.auth.subjectIsolation` config.
- `controller/cmd/controller/podspec.go:514-529` — agent pod NATS auth wiring.
- `controller/internal/config/config.go:40-65` — `NatsToken` / `NatsTokenSecret` / `NatsAgentUserSecret` fields.
- `kbeads/internal/eventbus/streams.go` — subject helpers; per-actor subjects exist as a naming convention.
- `coop/crates/cli/src/transport/nats.rs:150-233` — coop event publisher.
- `coop/crates/cli/src/transport/inbox.rs:209-234` — JetStream pull consumer.
- NATS docs: [Authorization](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/authorization) — templated subjects, JWT permissions, auth callout (2.10+).
