# ADR: Human Attribution Propagation Model (`operated_by`)

**Tracking bead:** ka-0000005586 (R3)
**Parent plan:** ka-0000003603 — `docs/research/human-attribution-operated-by.md`
**Folds in:** R5 (trust-model writeup)
**Status:** Proposed — research-only, no schema/code changes
**Author:** gardenio-r3-propagationmodel-adr-7yu, 2026-04-29

---

## 1. Context

Today every bead, comment, and event carries an **agent** identity in its `created_by` / `actor` / `author` field. The agent identity is set server-side from the authenticated caller (see `kbeads/internal/server/audit.go::actorFromContext` at lines 47–49 and `AuthIdentityFromContext` at lines 26–31).

We have **no first-class link from a bead to the human operator** who caused it to exist. The raw datum exists today (`Fields["slack_user_id"]` on the agent bead, set by the Slack bridge — `gardenio/controller/internal/bridge/bot_commands.go:531`), but it does not propagate to anything the agent subsequently creates.

The umbrella plan (ka-0000003603 §3 question 3) frames the propagation choice as three options:

- **(a) Inherited from spawning agent.** When agent A creates bead X, the server looks up A's own bead's `operated_by` and copies it onto X.
- **(b) Per-request header.** The agent sends `X-Operated-By: <id>` on every mutation. The server sanitizes and records it.
- **(c) Session-scoped.** `operated_by` is set at spawn, exposed via env var, and the agent's `kd`/`gb` CLI wrapper auto-attaches it as a header on every call.

This ADR picks one (with a documented fallback), commits to a trust model that mirrors the existing `created_by` / `X-Actor` story, and produces sequence diagrams for the three flows that matter.

## 2. Decision

**Adopt option (c) — session-scoped — with option (a) as a server-side fallback for missing headers and as a defense-in-depth check against a spoofed header.**

Concretely:

1. **At spawn time**, the controller writes `Fields["slack_user_id"]` onto the agent bead (already done at `bot_commands.go:531`) and additionally sets a `BEADS_OPERATED_BY` env var on the pod alongside the existing `BEADS_ACTOR` / `KD_ACTOR` (`gardenio/controller/internal/podmanager/manager.go:714-721`). For Slack-triggered agents the value is the raw Slack user ID (`U…`); for direct-CLI users it is the OIDC subject; for scheduled / autonomous agents the value is **empty**.
2. **The `kd` / `gb` CLI wrapper auto-attaches `X-Operated-By: <env value>`** as a header (HTTP) and gRPC metadata (`x-operated-by`) on every request, mirroring the existing `X-Actor` plumbing in `kbeads/cmd/kd/main.go:121` and `kbeads/internal/client/http.go:42-45,634` and `kbeads/internal/client/grpc.go:60-74`. When the env var is empty, no header is sent.
3. **The server resolves `operated_by` in a new helper `operatedByFromContext(ctx)`**, sibling to `actorFromContext` (`kbeads/internal/server/audit.go:47-49`):
   - If the caller authenticated with the primary static token, accept a sanitized `X-Operated-By` header (same trust boundary that already lets static-token callers set `X-Actor` — see `interceptors.go:247-255` for gRPC and `interceptors.go:525-531` for HTTP).
   - If no header was supplied, fall back to looking up the **calling agent's own bead** (resolved via `KD_AGENT_ID` → `BOAT_AGENT_BEAD_ID`, which the controller already sets at `manager.go:717`) and reading `Fields["slack_user_id"]`. This is option (a) as an inheritance safety net.
   - If both are empty, persist `NULL`. Scheduled / autonomous beads stay `NULL`.
4. **The server writes the resolved `operated_by` onto every mutation** that today writes `actor`/`created_by`/`author`: `CreateBead`, `AddComment` (`server.go:420`), `AddDependency` (`server.go:218`), `RecordEvent` (via `recordAndPublish`, `server.go:138-155`), and the close/assign/update paths.
5. **An agent code path that legitimately acts for a different operator within one session** (the gardener edge case, §6.1) calls a new `kd --operated-by <id>` flag (or `gb` equivalent) which overrides the env-derived header for that one invocation. The override is still subject to server-side sanitization and the same static-token trust boundary.

This is **(c) primary, (a) fallback** — not a free choice between them. The fallback exists because (c) alone fails open if the wrapper plumbing breaks; the inheritance path gives us a no-worse-than-today attribution even when a buggy CLI forgets the header.

## 3. Sequence diagrams

### 3.1 Slack-triggered case end-to-end

The default flow. A human types in Slack, the bridge spawns an agent, the agent creates a child bead. The human's Slack ID flows from Slack event → agent pod env → CLI header → server → DB.

```
┌───────┐    ┌────────────┐   ┌────────────┐   ┌────────┐    ┌──────────┐    ┌─────┐
│ Slack │    │ slack-bridge│  │ controller │   │ agent  │    │  kbeads  │    │  DB │
│ user  │    │             │  │ /podmgr    │   │  pod   │    │  daemon  │    │     │
└───┬───┘    └──────┬──────┘  └─────┬──────┘   └────┬───┘    └─────┬────┘    └──┬──┘
    │ /spawn …      │               │               │              │            │
    │ user=U0AGM…   │               │               │              │            │
    ├──────────────►│               │               │              │            │
    │               │ SpawnAgent(   │               │              │            │
    │               │   user=U0AGM…)│               │              │            │
    │               ├──────────────►│               │              │            │
    │               │               │ Bead.Create   │              │            │
    │               │               │ (agent bead,  │              │            │
    │               │               │  Fields[      │              │            │
    │               │               │   "slack_user │              │            │
    │               │               │   _id"]=U0A…) │              │            │
    │               │               ├──────────────────────────────►│           │
    │               │               │               │              │ INSERT     │
    │               │               │               │              ├──────────►│
    │               │               │ Pod env:      │              │            │
    │               │               │  BEADS_ACTOR= │              │            │
    │               │               │   <agent>     │              │            │
    │               │               │  KD_AGENT_ID= │              │            │
    │               │               │   kd-…        │              │            │
    │               │               │  BEADS_      │              │            │
    │               │               │   OPERATED_  │              │            │
    │               │               │   BY=U0AGM…  │              │            │
    │               │               ├──────────────►│              │            │
    │               │               │               │ kd comment … │            │
    │               │               │               │ X-Actor:     │            │
    │               │               │               │  <agent>     │            │
    │               │               │               │ X-Operated-  │            │
    │               │               │               │  By: U0AGM…  │            │
    │               │               │               ├─────────────►│            │
    │               │               │               │              │ resolve    │
    │               │               │               │              │ actor=     │
    │               │               │               │              │  <agent>   │
    │               │               │               │              │ operated_  │
    │               │               │               │              │  by=U0AGM… │
    │               │               │               │              │ INSERT     │
    │               │               │               │              ├──────────►│
    │               │               │               │              │            │
```

Bead row in DB: `created_by=<agent-name>`, `operated_by=U0OPERATOR2`. Comment / event rows: `actor=<agent-name>`, `operated_by=U0OPERATOR2`.

### 3.2 Direct-CLI use from a human laptop

A human runs `kd close kd-…` from their own machine, authenticated via OIDC. There is no agent in the loop; `operated_by` collapses onto the human's IdP subject — but it is set by the **server** from the authenticated identity, not by the client header.

```
┌───────┐                         ┌──────────┐         ┌─────┐
│ human │                         │  kbeads  │         │  DB │
│ kd    │                         │  daemon  │         │     │
└───┬───┘                         └─────┬────┘         └──┬──┘
    │ kd close ka-X                     │                  │
    │ Authorization: Bearer <oidc-jwt>  │                  │
    │ X-Actor: (not sent — OIDC path)   │                  │
    │ X-Operated-By: (not sent)         │                  │
    ├──────────────────────────────────►│                  │
    │                                   │ verifier →       │
    │                                   │  email=          │
    │                                   │   matt@acme.example │
    │                                   │ AuthIdentity{    │
    │                                   │  Type:"oidc",    │
    │                                   │  Subject:        │
    │                                   │   matt@acme.example}│
    │                                   │ actor=           │
    │                                   │  matt@acme.example  │
    │                                   │ operated_by=     │
    │                                   │  matt@acme.example  │
    │                                   │ (server collapse)│
    │                                   │ UPDATE           │
    │                                   ├─────────────────►│
```

For OIDC-authenticated callers `actor == operated_by` (a human is doing the work directly). The server applies this rule once, in `operatedByFromContext`, by checking `AuthIdentity.Type == "oidc"` and copying `Subject`. No client-side change needed.

### 3.3 Scheduled / autonomous agent (no operator)

A scheduled gardener (cron-driven) spawns and starts working. There is no human in the spawn path. `BEADS_OPERATED_BY` is empty in the pod env, the wrapper sends no header, and the server's fallback (the agent bead's `Fields["slack_user_id"]`) is also empty. `operated_by` is `NULL` for everything the scheduled agent creates.

```
┌──────────┐    ┌────────────┐   ┌────────┐    ┌──────────┐    ┌─────┐
│ scheduler│    │ controller │   │ agent  │    │  kbeads  │    │  DB │
│ (cron)   │    │ /podmgr    │   │  pod   │    │  daemon  │    │     │
└────┬─────┘    └─────┬──────┘   └────┬───┘    └─────┬────┘    └──┬──┘
     │ schedule fires │                │              │            │
     ├───────────────►│                │              │            │
     │                │ Bead.Create    │              │            │
     │                │ (agent bead,   │              │            │
     │                │  no slack_user │              │            │
     │                │  _id field)    │              │            │
     │                ├───────────────────────────────►            │
     │                │ Pod env:       │              │            │
     │                │  BEADS_       │              │            │
     │                │   OPERATED_   │              │            │
     │                │   BY=""       │              │            │
     │                ├───────────────►│              │            │
     │                │                │ kd create …  │            │
     │                │                │ X-Actor:     │            │
     │                │                │  <agent>     │            │
     │                │                │ (no X-       │            │
     │                │                │  Operated-By)│            │
     │                │                ├─────────────►│            │
     │                │                │              │ resolve:   │
     │                │                │              │  no header,│
     │                │                │              │  agent     │
     │                │                │              │  bead has  │
     │                │                │              │  no        │
     │                │                │              │  slack_   │
     │                │                │              │  user_id   │
     │                │                │              │ operated_ │
     │                │                │              │  by=NULL  │
     │                │                │              │ INSERT     │
     │                │                │              ├───────────►│
```

`operated_by` `NULL` is a legitimate, queryable state — `kd list -f operated_by=` filters for autonomous work — and is what the schema sketch in R4 should make nullable by default.

## 4. Trust model (folds in R5)

This section is the R5 deliverable.

### 4.1 Where the actor is decided today

Three call sites set actor server-side:

- `kbeads/internal/server/audit.go:47-49` — `actorFromContext(ctx)` returns `AuthIdentity.Subject`.
- `kbeads/internal/server/interceptors.go:223-256` (gRPC) and `:498-532` (HTTP) — the auth interceptor decides the `AuthIdentity` for each call:
  - **OIDC path**: `Subject = <email>` from the JWT (gRPC `:273,283`, HTTP `:554-573`). Forward-auth fallback at HTTP `:554-557, 569-571` reads `X-Auth-Request-Email`.
  - **Static token path** (the agent pods, the controller, the bridge): `Subject = "static"` initially (gRPC `:225`, HTTP `:501`); then **only static-primary-token callers may override `Subject` with a sanitized `X-Actor` header** (gRPC `:249-255`, HTTP `:527-531`).
  - **Fleet token path**: `Subject = <fleet-name>` (gRPC `:229`, HTTP `:505`). Fleet tokens may not override via `X-Actor` (the override block is gated on `id.Type == "static"`).
- `sanitizeActorHeader` (`audit.go:58-69`): trims, length-caps at 128, rejects control characters.

The audit invariant from `audit.go:33-46` is: **the server, not the client, decides the actor for non-static callers.** Static-primary-token callers (a small, trusted set: agent pods, controller, bridge) are the one exception, and the comment explicitly notes that they "already have full write access, so allowing them to self-identify for audit attribution stays inside the §C3 trust boundary." This is the same boundary `operated_by` will inherit.

### 4.2 Where `operated_by` will be decided

Mirroring §4.1, in a new `operatedByFromContext(ctx) string`:

| Caller auth | Source of `operated_by` |
|---|---|
| OIDC | copy `AuthIdentity.Subject` (the human is the operator). |
| Fleet token | empty (fleet tokens are service identities; the human-operator notion does not apply). Future: a fleet may explicitly opt in to passing `X-Operated-By`, but v1 ignores it. |
| Static primary token | sanitized `X-Operated-By` header (identical sanitization rules to `X-Actor`). If absent, fallback: load the calling agent's bead by `KD_AGENT_ID` → bead-by-id, return `Fields["slack_user_id"]`. If still absent, return `""`. |
| Anonymous / `none` | empty. |

The fallback path requires a single bead read per request. In practice we cache the (agent-id → operated_by) map for the lifetime of the agent's session — the server already has all the infra for this (the agent bead is the same bead the existing roster/kill paths read repeatedly). The cache is invalidated on bead updates via the existing event bus.

### 4.3 Sanitization

`sanitizeOperatedByHeader` is a strict alias of `sanitizeActorHeader` (`audit.go:58-69`):

- Trim whitespace, reject empty.
- Length ≤ 128 (Slack `U…` IDs are 11 chars; emails ≤ 254; 128 covers both with margin).
- Reject any rune `< 0x20` or `== 0x7f` (control chars, including CR/LF/TAB).

We do **not** validate the format of the value (`U[A-Z0-9]+` for Slack, RFC 5321 for email) at this layer — that lives in the lookup tables and `kd list -f operated_by=` query path. Same precedent as `X-Actor`, which currently accepts any sanitized opaque string.

### 4.4 Spoofing analysis

Threat: a static-token caller (e.g. a buggy or compromised agent pod) sets `X-Operated-By: matt@acme.example` to attribute its mutations to a different human.

Today's `created_by` story under the same threat: the same caller can set `X-Actor: matt@acme.example` to spoof `created_by`. We accept this because the static primary token already grants full write access to the database — an attacker with that token can rewrite history directly. The audit comment at `audit.go:33-46` and `interceptors.go:247-249` formalizes this: static-primary-token callers are inside the trust boundary; we only defend against unauthenticated and OIDC-authenticated callers spoofing.

`operated_by` inherits the same property: an OIDC caller cannot spoof `operated_by` (the server overrides their header from the JWT subject). A static-token caller can — and that is no worse than the existing `X-Actor` story. The defenses are token rotation (which we have, see `kbeads/internal/store/postgres/migrations/007_revoked_tokens.up.sql`) and the audit log of `created_by` itself: a spoofed `operated_by` still has the (un-spoofable) agent identity in `created_by` next to it, so a human looking at the bead can spot the mismatch.

A second, weaker threat: agent A (legitimately operated by Alice) reads agent B's bead, finds `Fields["slack_user_id"]=Bob`, and uses some imagined "set-operated-by-by-bead-id" API to forge attribution. We do not expose such an API. The only paths that set `operated_by` are §4.2; none of them accept a bead-id-based override.

### 4.5 What the server vs the client decides

| Decision | Decided by | Why |
|---|---|---|
| Whether the call is authenticated and who the agent is | server (interceptor) | unchanged from today; `AuthIdentity` |
| `actor` / `created_by` | server (`actorFromContext`) | unchanged; AUDIT-2026-04-18 §C3 |
| `operated_by` for OIDC callers | server (copy from `AuthIdentity.Subject`) | a human's session is the source of truth for who the human is |
| `operated_by` for static-token callers | server, with client hint | header is a hint, sanitized and gated to the static-token trust boundary |
| `operated_by` fallback when header missing | server (read agent bead's `slack_user_id`) | the spawn-time fact is server-trusted; the bead itself is server-written |
| Filtering / display | client | `kd list -f operated_by=…`, beads-viewer column |

Crucially, **the client's `kd`/`gb` wrapper is never the trust anchor**. If the wrapper omits the header, the server falls back to the agent bead's spawn-time field. If the wrapper sends a wrong header, the server still gates acceptance behind the static-token check. In every path the server is the final arbiter.

## 5. Rationale: why (c) over (a) and (b)

**Why not (a) alone — pure inheritance.** Two reasons.

First, it requires a per-request bead read for *every* mutation to look up the agent's `slack_user_id`. The recordAndPublish hot path (`server.go:138-155`) runs on every comment, dep, event. Adding a synchronous bead read there is a meaningful regression we'd then need to optimize with a cache; (c) avoids the read in the common case (header present).

Second, (a) collapses the "gardener serving two operators" case (§6.1) to whichever operator was first. The gardener bead has one `slack_user_id`, and inheritance would attribute all of its later work to that single human — even when the gardener is now servicing a different human's thread. Per-request override is the only clean answer.

**Why not (b) alone — pure per-request header.** It's brittle. Every code path that talks to the daemon (Go controller, kd CLI, gb CLI, beads-viewer-via-server, future SDKs) would have to remember to attach the header. We've already lived through this with `X-Actor`: the `gb` CLI gained it late, agent pods were emitting `static` for `created_by` as a result (`interceptors.go:247-249` calls this out as a fixed bug), and the fix was the same env-var-driven plumbing we are recommending here for `operated_by`. Doing (b) by hand would re-invite that bug class.

**Why (c) — session-scoped via env + auto-attached header.** It piggybacks on plumbing that already exists (`KD_ACTOR`, `BEADS_ACTOR`, the wrapper's `SetActor` call) and that already proved out the pattern. The agent pod is already a single-operator entity 99% of the time; setting it once at spawn covers that case for free. The override flag handles the gardener case. The server-side fallback to the agent bead handles the "wrapper dropped the header" case without our needing to trust the wrapper.

**Why a fallback is worth the bead-read cost in the rare case.** Two reasons. (1) The fallback only fires when the header is missing — i.e. a buggy or pre-rollout client — so the cost lands on the failure path, not the hot path. (2) Pre-cutover beads (where pods exist without `BEADS_OPERATED_BY` in their env) keep working without a coordinated controller-and-pod restart; the daemon can attribute their mutations from the existing `slack_user_id` field. This is the same migration ergonomic that R7 (backfill) relies on.

## 6. Edge cases

### 6.1 A gardener handling two Slack threads in one session

A long-running gardener pod is spawned by Alice's thread, but later reacts to a mention in Bob's thread (via `gb slack thread` or a NATS-driven mention handler). The gardener's pod env has `BEADS_OPERATED_BY=Alice`, but the work it's now doing is for Bob.

**Resolution under (c) + override:** The mention-handler code path in the gardener calls (effectively) `kd create … --operated-by=Bob` for the mutations that arose from Bob's thread. The wrapper translates `--operated-by` into `X-Operated-By: Bob`. Server accepts (static-token, sanitized). Bead is correctly attributed.

**What to enforce in code review:** any code path in `gardenio/controller/internal/bridge/bot_*.go` that resolves a Slack `ev.User` and *does not* propagate that user ID into the `kd`/`gb` calls it makes is a bug. R6 (API surface) should specify the per-call override flag and the gardener-side wrapper's expectation.

**Fallback behavior if the override is forgotten:** the server falls back to the gardener's own bead `Fields["slack_user_id"]`, which is Alice. The bead is mis-attributed to Alice. This is observable (Bob says "I asked for X but it's logged as Alice's") and recoverable (`UpdateBeadFields`). It is no worse than today, where everything is attributed to the agent and no human is named at all.

### 6.2 An agent triggering another agent

Agent A (operated by Alice) spawns agent B via the existing controller spawn path. B's pod env should inherit `BEADS_OPERATED_BY=Alice` (from A's view of "who am I working for") so that B's mutations attribute to Alice too — not to A's agent name and not to nobody.

**Resolution.** The spawn API (controller `SpawnAgent`, `bot_commands.go:515`) needs to read the spawning agent's `operated_by` (from request context) and propagate it onto the new agent bead's `Fields["slack_user_id"]` — the same field the bridge writes for Slack-spawned agents. The pod-build code at `manager.go:714-721` already plumbs that field into the env. R6 (API surface) should make this explicit; this ADR commits to the propagation rule but does not specify the spawn-API shape.

**Trust note.** A spawning by an OIDC caller (a human running `kd spawn`) sets `operated_by` from the JWT subject — same as §3.2. A spawning by a static-token caller (the bridge, the controller) propagates whatever the spawner's resolved `operated_by` was. There is no path by which a static-token caller spawns an agent attributed to an OIDC subject the spawner cannot prove — because the spawner's own `operated_by` was already resolved under §4.2, so the trust gate has already fired upstream.

### 6.3 Decisions: who answered the decision

`Fields["requested_by"]` is mentioned in §3 question 6 of the parent plan. v1 keeps it as-is (the existing `decision_response.responder` field carries the answering-human identity at decision-resolution time). When R6 designs the decision API, it should set `operated_by` on decision *creation* from the agent's session (per §4.2) and on decision *resolution* from the responder (a human running `kd decision report`, which is the OIDC path → `Subject` → `operated_by`).

### 6.4 Wisp / formula / mol fan-out

`kd formula apply` and friends create many beads in a single API burst. They run with the caller's `operated_by` for all of them, not per-step overrides. This is the right default — the human running `formula apply` is the operator for the whole expansion. If a future use case needs per-step override, it goes through the same `--operated-by` flag the gardener case uses.

### 6.5 Backfill (handed off to R7)

R7 designs the migration that walks existing beads and copies `Fields["slack_user_id"]` from agent beads onto every descendant bead the agent has created. This ADR commits to the schema being nullable (so pre-backfill rows can coexist with NULL `operated_by`) and to the backfill being idempotent and bounded. R7 owns the actual plan.

## 7. Consequences

**Positive:**

- The hot mutation path (`recordAndPublish`) gains zero extra DB reads in the common case. The header carries the value; the server only sanitizes and writes.
- The trust model is identical to the existing `X-Actor` story; no new auth invariants to reason about. Reviewing the existing audit unit tests (`xactor_test.go`, `whoami_test.go`, `rate_limit_test.go`) gives a template for what `operated_by` tests look like.
- The plumbing reuses existing infrastructure: `BEADS_ACTOR` ↔ `BEADS_OPERATED_BY`; `SetActor` ↔ `SetOperatedBy`; `--actor` ↔ `--operated-by`. Each touch point is a one-line analog.
- Scheduled / autonomous agents naturally produce `NULL` — there is no need to invent a sentinel value.

**Negative:**

- We add one new env var, one new HTTP header, one new gRPC metadata key, and one new CLI flag. Each is a small surface, but they multiply across docs, agent images, and helm charts. R6 should enumerate the touch points.
- The fallback path (read agent bead by ID, return `Fields["slack_user_id"]`) introduces a per-mutation bead read on the failure path. We need a small in-memory cache keyed by `(agent_bead_id)` with event-bus invalidation. The cache is bounded by the number of live agents (low hundreds today).
- The gardener-multi-operator case requires the gardener's mention-handling code to remember the override flag. R6 should make it impossible to forget by funneling all `kd`/`gb` calls through a gardener-context helper that reads the current Slack-thread user.

**Neutral:**

- v1 stores `operated_by` as a `TEXT` opaque ID (Slack `U…` or OIDC email). R2 (identity-shape ADR) is the canonical decision on whether this becomes a FK; this ADR's plumbing is shape-agnostic — if R2 lands as TEXT, no change; if R2 lands as FK, the same propagation path delivers the same string and the FK lookup happens at the storage layer.

## 8. Alternatives considered (and rejected)

- **Server-only inheritance with a per-mutation bead lookup (option (a) alone).** Rejected: hot-path cost; fails the gardener case (§6.1).
- **Client-only header with no server fallback (option (b) alone).** Rejected: brittle, replays the historical `X-Actor` bug.
- **A new "humans" or "operators" table, with `operated_by` as a FK from day one.** Deferred to R2. This ADR is shape-agnostic — see §7 neutral note.
- **Replacing `created_by` with a `(created_by_agent, created_by_human)` pair.** Rejected per parent plan §7 (out-of-scope) — adding alongside is the right migration story.

## 9. Open follow-ups

- **R6** owns the `--operated-by` flag, the spawn-API propagation in §6.2, and the gardener-context helper in §6.1.
- **R7** owns the backfill plan referenced in §6.5.
- **R4** owns the schema migration that adds the `operated_by TEXT NULL` columns this ADR's resolver writes to.
- **R2** owns the identity-shape decision; this ADR is compatible with both TEXT and FK outcomes.

---

## 10. File:line citations (full audit trail)

- Server-side actor decision: `kbeads/internal/server/audit.go:47-49` (`actorFromContext`), `audit.go:26-31` (`AuthIdentityFromContext`), `audit.go:33-46` (the spoofing comment).
- Sanitizer: `kbeads/internal/server/audit.go:58-69` (`sanitizeActorHeader`).
- gRPC interceptor — static-token path: `kbeads/internal/server/interceptors.go:223-258` (token match, fleet/static identity injection, `X-Actor` override at `:247-255`).
- gRPC interceptor — OIDC path: `kbeads/internal/server/interceptors.go:260-289` (RoleExtractor, identity injection at `:273,283`).
- HTTP interceptor — static-token path: `kbeads/internal/server/interceptors.go:498-535` (`X-Actor` override at `:525-531`).
- HTTP interceptor — OIDC path: `kbeads/internal/server/interceptors.go:537-579` (forward-auth email fallback at `:554-557, 569-571`).
- Bead creation actor write: `kbeads/internal/server/server.go:218` (`AddDependency.CreatedBy`), `server.go:420` (`AddComment.Author`).
- Event recording: `kbeads/internal/server/server.go:138-155` (`recordAndPublish`, used by every mutation).
- CLI actor flag plumbing: `kbeads/cmd/kd/main.go:18,24-36,121` (`--actor`, `KD_ACTOR`, `defaultActor`, registration).
- CLI client `X-Actor` header: `kbeads/internal/client/http.go:42-45,634` (`SetActor`, `req.Header.Set("X-Actor", c.actor)`).
- gRPC client `x-actor` metadata: `kbeads/internal/client/grpc.go:60-63,67-74` (`SetActor`, `withFleet` adds `x-actor`).
- Slack bridge → agent spawn `slack_user_id` write: `gardenio/controller/internal/bridge/bot_commands.go:531` (`fields["slack_user_id"] = cmd.UserID`).
- Slack bridge mentions write: `gardenio/controller/internal/bridge/bot_mentions.go:725,796`, `bot_reactions.go:216`.
- Slack bridge thread / kill `slack_user_id` propagation: `gardenio/controller/internal/bridge/bot_agent_kill.go:199,512`, `bot_thread_test.go:454,512`.
- Decision notification routing by `slack_user_id`: `gardenio/controller/internal/bridge/bot_decisions.go:37-48`, `bridge/decisions.go:543-544`.
- Pod env injection at spawn: `gardenio/controller/internal/podmanager/manager.go:714-721` (`BEADS_ACTOR`, `KD_ACTOR`, `KD_AGENT_ID`, `BOAT_AGENT_BEAD_ID`).
- Migrations directory (where R4's `00X_human_attribution.up.sql` will land): `kbeads/internal/store/postgres/migrations/`. Latest in series: `007_revoked_tokens.up.sql`.
