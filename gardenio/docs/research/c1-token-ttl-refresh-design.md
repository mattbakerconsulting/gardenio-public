# C1 — Per-identity scoped tokens with TTL and refresh-token flow

**Status:** Design ADR / scope analysis. No implementation. Operator decisions needed before code starts.
**Tracks:** [ka-0000006930](https://beads-viewer.gardenio.e2e.example.com/ka-0000006930) — Epic C [ka-0000005919](https://beads-viewer.gardenio.e2e.example.com/ka-0000005919) (internet-safe daemon).
**Author:** sched-local-agent-security-epi-93hj.
**Date:** 2026-04-29.
**Predecessors (read these first):**
- `gardenio/docs/local-agent-epic-brief.md` — Epic A/B/C handoff brief.
- `gardenio/docs/per-agent-tokens-enablement-plan.md` — PAT rollout plan (parked, gated on public-internet exposure).
- `gardenio/docs/research/per-agent-tokens-codebase-state.md` — what exists in the codebase today.

## TL;DR

C1's brief description ("tokens are opaque, never expire, no refresh; design and implement short-lived tokens with refresh") **partially overlaps with already-shipped per-agent-token (PAT) machinery** in the controller. The actual remaining scope, given PAT exists, is narrower than the bead body implies and forks on three design questions the operator must call:

1. **Should TTL apply uniformly to all fleet tokens, or only to non-PAT tokens?** PAT tokens are already lifecycle-bound to a pod (revoked on pod done). Adding TTL on top is belt-and-suspenders. The TTL win is for *operator-static* tokens (admin, bridge, scheduler) that today never expire.
2. **What is the "identity" in "per-identity"?** Today fleet tokens carry `Fleet + Project + AllowedTypes + AllowedOps`, but no subject. PAT tokens are per-pod. C2 audit attribution work (closed) leans on `X-Actor` headers, not token identity. Embedding subject in the token vs. continuing the X-Actor pattern is a real fork.
3. **Refresh-token flow target audience.** Refresh flows are valuable for clients that hold a token across a TTL boundary. Pods are short-lived (no boundary). Operators and bridges hold long-lived tokens (refresh helps). Slack-bridge-style daemons would need refresh-or-rotate logic. This determines whether refresh is a daemon-server feature, a controller-shipped sidecar pattern, or both.

**Recommendation: write code only after the operator picks a scope (decision filed alongside this MR).** This doc names what's already done, what's still open, and three minimum-viable shapes for the implementation.

## 1. What's already shipped (do not re-build)

| Capability | Where | Status |
|---|---|---|
| Per-agent token issuance at pod spawn | `agenttoken.go:50+` (`provisionAgentToken`) called from `main.go:805,:875` | **Done.** Gated by `PER_AGENT_TOKENS` env (default `false`). |
| Project-scoped fleet tokens | `fleet.go:143+` (`RegisterTokenWithProject`) | **Done.** |
| Type-scope + op-scope on fleet tokens | `fleet.go:151+` (`RegisterTokenWithScopes`); enforced at handlers per C2 (ka-0000006767, closed) | **Done.** |
| Persistent revocation (hash-based) | `token_revocation.go` + `revoked_tokens` table; per C3 (ka-0000007026, closed) | **Done.** |
| Token persistence (survive daemon restart) | Config table key `fleet_token:<fleet>` (`http_fleet_tokens.go:48-67`) | **Done.** |
| Token registration API for admin | `POST /v1/fleet-tokens` (`http_fleet_tokens.go`) | **Done.** Admin-only via primary auth token. |
| Per-pod token cleanup on done | `revokeAgentToken` in `agenttoken.go`; called from controller's pod-done path | **Done.** |

**Implication:** Roughly half of the brief's "design and implement short-lived tokens" is already there as a per-pod lifecycle. The remaining questions are about the *non-pod-bound* tokens (admin, bridge, scheduler, future per-human tokens).

## 2. What's still open (the C1 surface)

### 2.1 TTL field on fleet tokens

Today `fleetTokenInfo` (`fleet.go:23-29`) has no `ExpiresAt`. The auth interceptor never checks expiry. PAT pods don't need TTL because they're revoked on pod done — but **admin / bridge / scheduler / human-operator tokens never expire and never rotate**. Compromise of those tokens today is permanent until a human notices and revokes by hash.

Surface area:
- `fleetTokenInfo` adds `ExpiresAt time.Time` (zero = never expires, for back-compat with seed/admin tokens).
- Token persistence adds the field to the config-store JSON.
- Auth interceptor checks expiry **before** revocation cache lookup.
- `POST /v1/fleet-tokens` accepts `ttl` (duration) or `expires_at` (RFC3339).
- A small reaper sweeps expired tokens out of the in-memory cache (cosmetic; expired tokens already fail the auth check).

### 2.2 Refresh-token flow

Refresh flow design = "issue a long-lived refresh token alongside a short-lived access token; access token's TTL is short (minutes-to-hours); refresh token can be exchanged for a new access token without re-presenting credentials."

Surface area:
- New endpoint: `POST /v1/auth/refresh` taking the refresh token, returning a fresh access token.
- Refresh tokens themselves persist (DB-backed, with their own TTL — typically days-to-weeks).
- Revocation of a refresh token must invalidate any access tokens issued from it (or accept a small window of "leaked refresh produces N more access tokens until they expire").
- Client-side: callers must handle 401 → refresh → retry. Affects `kd` CLI, beadsapi client, controller, slack-bridge, scheduler.

**This is the load-bearing question:** which clients actually need refresh, vs. which can re-register-on-restart? See §3 for the three shapes.

### 2.3 Per-identity claims

"Per-identity" in the bead body is ambiguous. Today:

- Fleet tokens carry `Fleet + Project + AllowedTypes + AllowedOps`. No `Subject`.
- Audit attribution uses `X-Actor` header, set by the controller per-pod. Bridges and the scheduler set `X-Actor` to a hard-coded string (or empty — see codebase-state §7.7).
- "Who created this bead?" is `X-Actor`, not the token bearer.

Adding a subject claim to the token would let the daemon **derive** `created_by` from the token, eliminating the X-Actor-spoofing class of bug. But it makes tokens longer-lived per identity and forces a refresh story (because rotating a per-human token without a refresh flow logs the human out everywhere).

This conflicts with the existing "PAT is per-pod" pattern: per-pod tokens carry no human subject, just a per-pod `agent-<beadID>` label.

## 3. Three minimum-viable shapes for C1

### Shape A — TTL only, no refresh, no subject change

**Scope:** Add `ExpiresAt` to fleet tokens. Auth interceptor enforces it. `POST /v1/fleet-tokens` accepts `ttl`.

**Coverage:**
- Operator/admin tokens get rotation via cron (operator re-issues every N days).
- PAT keeps working; per-agent tokens get a TTL slightly longer than expected pod runtime (e.g. 24h for safety).
- Bridges/scheduler need restart-on-rotate.

**Cost:** ~1 MR, ~300-500 LOC including tests. No client changes.

**Trade-off:** No refresh = restart-to-rotate. For the bridge/scheduler that's fine (they restart on config change anyway). For long-running operator CLI sessions, it's a UX paper-cut.

### Shape B — TTL + refresh on a single endpoint

**Scope:** Shape A, plus `POST /v1/auth/refresh` and a refresh-token model. Refresh tokens are persisted in their own table; access tokens stay in the existing fleet-token map but with TTL.

**Coverage:**
- Operator CLI (`kd`) holds refresh; auto-refreshes on 401.
- Bridges/scheduler can use refresh too, but it's lower-value than Shape A.
- PAT: optional — controller could issue refresh on pod spawn so a long-running pod can rotate access tokens without controller round-trips. Likely not worth it for v1.

**Cost:** ~2-3 MRs. Daemon-side refresh endpoint + refresh table + revocation cascade. Client-side `kd` and beadsapi changes. ~1500 LOC.

**Trade-off:** Real refresh-flow plumbing across multiple clients. Higher correctness bar (revocation cascade is easy to get wrong).

### Shape C — Shape B + per-identity subject claim, deprecate X-Actor

**Scope:** Shape B, plus add `Subject` to fleet-token model. Daemon derives `created_by` from token subject if `X-Actor` is unset/spoofed. PAT controller sets subject to `agent:<beadID>`. Operators/bridges set subject to their identity.

**Coverage:** Closes the X-Actor-spoofing class of bug.

**Cost:** ~3-4 MRs. Schema change for fleet-token persistence (add Subject column or JSON field). Audit attribution refactor. Controller PAT change. Test suite for subject-vs-X-Actor precedence.

**Trade-off:** Largest change. Touches the audit/attribution surface that B-series work just stabilized — risk of regressing the closed B work. Probably worth a separate epic spawn from C1.

## 4. Recommendation

Pick **Shape A** for C1 (ka-0000006930). Spawn **Shape B as a child task** if/when the operator wants refresh. Spawn **Shape C as a separate epic** since it overlaps the closed B-series attribution work and deserves its own design pass.

Reasons:
- Shape A is the single highest-value bit: it removes "compromised token = forever-valid" from the threat model.
- Shape B is medium-value but requires touching every client; better deferred until there's a concrete client pain point.
- Shape C is a different problem (attribution, not authn) and deserves to ride alongside or after the B-series work it would refactor.

## 5. Implementation sketch for Shape A (if approved)

1. **Model change** (`fleet.go`):
   - Add `ExpiresAt time.Time` to `fleetTokenInfo`.
   - Add `RegisterTokenWithExpiry(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time)`.
   - Add `IsExpired(token string) bool`.

2. **Persistence** (`http_fleet_tokens.go`):
   - `registerFleetTokenRequest` adds `TTL string` (duration) and/or `ExpiresAt string` (RFC3339).
   - Persisted JSON adds `expires_at` field; load path reads it.
   - On daemon startup, expired tokens are dropped from the in-memory map (and optionally deleted from config-store; see §6.1).

3. **Auth check** (interceptor / middleware):
   - Before existing scope checks, call `IsExpired`. Return 401 with `WWW-Authenticate: Bearer error="invalid_token", error_description="token expired"`.

4. **Reaper** (optional, cosmetic):
   - Background goroutine sweeps `tokenFleets` every minute, removes expired entries.

5. **CLI**:
   - `kd fleet token register --ttl=24h ...` (proposed CLI, see ka-0000000937).

6. **Tests**:
   - Unit: `IsExpired` returns true after wall-clock advance (use a `now func() time.Time` for testability).
   - Handler-level: expired token gets 401, not 403.
   - Persistence: round-trip an expired token through `SetConfig`/load.
   - Backward-compat: tokens persisted without `expires_at` field load as zero `ExpiresAt` (= never expires).

## 6. Open design points (please decide before code)

### 6.1 Should expired tokens be removed from the config store?

Two options:
- **Yes (delete on expire):** Keeps the store clean. But racy with re-registration: if an admin re-uses the same token string with a fresh TTL, a delete-then-insert during expiry could race.
- **No (keep, just fail auth):** Simpler. Slightly larger config table over time. Periodic cleanup job at most.

**Recommendation:** Keep. The cleanup risk-vs-reward isn't worth it for v1.

### 6.2 Do PAT tokens get TTL?

The controller could call `RegisterTokenWithExpiry` with `ttl = expected-pod-runtime + 1h` or similar. Defends against "pod stops calling done but token stays alive forever" failures.

**Recommendation:** Yes, with a generous buffer (24h default). The controller's revoke-on-done path stays primary; TTL is the safety net for failed revocation.

### 6.3 What's the default TTL when none is specified?

- For operator-static tokens registered manually: probably opt-out (zero = never expires) for back-compat.
- For PAT-issued tokens: 24h.
- For seed tokens (env-loaded `BEADS_FLEET_MAP`): zero (operator-owned, rotated by env-var rotation).

### 6.4 Backward-compat for existing tokens at upgrade time

All existing fleet-token rows in the config table have no `expires_at` field. On daemon start, JSON unmarshal yields zero `ExpiresAt`, which `IsExpired` should treat as **never expires**. This preserves the current behavior. No migration needed.

## 7. Out-of-scope for this MR

- Refresh-token flow (Shape B; spawn child task).
- Per-identity subject claims (Shape C; spawn separate epic).
- TLS / public-internet exposure (ka-0000001861; that epic gates PAT enablement, not C1).
- NATS subject scoping (C6, ka-0000005594; separate task under same Epic C).

## 8. Questions for the operator (filed as `gb decision`)

1. Approve Shape A as the C1 v1?
2. Default TTL policy: zero for operator-static, 24h for PAT, zero for env-seed — agreed?
3. Should expired tokens be deleted from the config store (§6.1), or kept but fail auth?

If the operator picks a different shape, this doc gets updated and the implementation MR follows.
