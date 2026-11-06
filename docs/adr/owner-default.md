# ADR — Owner default at bead create-time

> Status: **Accepted** (decision ka-0000003484 closed with choice
> `[keep-header-but-tighten]`)
> Bead: ka-0000003507 (C1 of parent task ka-0000008031)
> Parent epic: ka-0000008007 — operated_by multi-user readiness
> Supersedes: ka-0000000058 (the prior "spawn-chain-root-only default" ADR)
> Original source: ka-0000000269 (the "default Owner ← X-Operated-By verbatim" hook)
> Operator: Matt Baker (slack:U0OPERATOR1)

## Context

ka-0000000269 introduced server-side defaulting in
`kbeads/internal/server/beads.go` CreateBead: when `req.Owner` is empty and
the static-token caller passed an `X-Operated-By` header, the server defaults
`bead.Owner = X-Operated-By` (gated behind `BEADS_OWNER_DEFAULT_ENABLED`).

After enabling, the practical effect was a disaster (Slack thread
2026-05-20, ka-0000007020): Matt drives most spawns, most spawn chains
carry his `slack:U0OPERATOR1` forward at every hop because no spawn site
rewrites the header, and >50% of new beads ended up `owner =
slack:U0OPERATOR1`. Beads-viewer collapses ownership to a single human;
"find my work" becomes meaningless for anyone but Matt.

A first attempt at remediation (ka-0000000058) replaced the verbatim
default with a *spawn-chain-root-only* policy: walk the agent's
`spawned_by` chain to its root, extract the human identity from the root
agent's fields, and use that as Owner — header alone never sufficed.
Decision ka-0000003484 (Matt) rejected that design as too aggressive a
swing: the chain walk on every write is expensive, the override-only
escape hatch forces every bridge to relearn ownership it already
forwarded via `X-Operated-By`, and the common case ("the header IS the
right human, the chain is short and clean") loses correctness because we
refuse to attribute even when the header and the chain agree.

## Decision

Keep the ka-0000000269 `X-Operated-By → Owner` default, but **tighten**
it so the disaster shape (mid-chain relay carrying a stale header) can
no longer drive an Owner assignment.

### Policy (server-side, CreateBead)

Evaluated in `applyOwnerDefault(ctx, req.Owner)`:

1. **Caller-supplied `Owner` always wins.** `req.Owner != ""` (and not
   the `-` sentinel) persists as-is, no defaulting.

2. **`-` sentinel persists as empty string.** Already in place; preserved.

3. **`X-Owner-Override: <value>` header wins next.** Trusted-bridge
   escape hatch (e.g. jira-bridge attributing to a ticket reporter, a
   Slack `/spawn` from a non-Matt user where the bridge knows the right
   human). Sanitized into ctx by the static-token interceptor; honored
   even with the feature flag off because the override is an explicit
   bridge signal. Preserved from the ka-0000000058 design.

4. **`X-No-Owner-Default` opt-out and `BEADS_OWNER_DEFAULT_ENABLED`
   flag.** When the opt-out is set, or the flag is off, no default fires
   and Owner stays empty. Preserved from ka-0000000269.

5. **Default from `X-Operated-By` — tightened.** When none of the above
   short-circuit and the flag is on:

   - The header value MUST be a *human-with-email* identity per the
     ka-0000006809 Invariant — i.e. `slack:U<alnum>` or
     `oidc:<local>@<domain>`. Fleet (`fleet:*`), system (`system:*`),
     bare-agent, `sched-*`, and `thread-*` shapes do NOT qualify. (The
     `sanitizeOperatedByHeader` path already rejects these at the static-
     token interceptor; this layer re-checks for defence in depth.)
   - **For agent-driven writes** — i.e. the caller's AuthIdentity is
     `static` with a specific agent subject (not the bare `"static"`
     primary-token subject) — the resolved header value MUST equal the
     spawn-chain ROOT human identity, computed by walking the calling
     agent's `fields.spawned_by` chain up to the root agent bead and
     reading its `fields.user_bead_id` / `fields.slack_user_id` /
     `fields.operated_by`. A mismatch (i.e. "the relayed header doesn't
     match who actually started this chain") fails closed: Owner stays
     empty and the server emits a structured `owner_default_mismatch`
     audit-log warning so dashboards can quantify how often the
     Matt-disaster shape would have fired.
   - **For non-agent static-token callers** — i.e. AuthIdentity
     `subject == "static"` (a bridge or admin using the primary token
     directly, with no spawn chain to walk) — the human-with-email check
     is sufficient. The static-token surface is already R3-trusted to
     self-attribute, and there is no agent chain to compare against.

6. **Otherwise, Owner stays empty.** Beads-viewer renders `(unowned)`
   via the `system:unowned` sentinel pattern from ka-0000004566.

### Acceptance

- A direct OIDC- or primary-static-token write with `X-Operated-By:
  slack:U…` (or `oidc:…`) and no `req.Owner` persists with `owner =
  <header>` (the ka-0000000269 happy path is preserved for non-agent
  callers).
- A direct write with `X-Operated-By: fleet:slack-bridge` (or
  `system:*`, or any non-human shape) persists with `owner = ""`.
- An agent write where `X-Operated-By` matches the agent's spawn-chain
  ROOT human persists with `owner = <header>`.
- An agent write where `X-Operated-By` is a relayed header that does
  NOT match the agent's spawn-chain root (the Matt-disaster shape)
  persists with `owner = ""`, and the server emits an
  `owner_default_mismatch` structured warning naming the agent, header
  value, and resolved root.
- `X-Owner-Override` continues to win over the chain check.
- The `-` sentinel and `X-No-Owner-Default` opt-out continue to
  short-circuit ownership.

### What this does NOT change

- `bead.OperatedBy` / `bead.OperatedByID` are unaffected. Those continue
  to record the audit operator (ka-0000006809 R2 fail-closed). The split
  between "audit operator" and "owner" is intentional and preserved —
  this policy only narrows when *Owner* defaults fire.
- The `applyAutoInherit` path (ka-0000006800) for
  `Fields[operated_by]` / `Fields[user_bead_id]` is unaffected.
- The C4 rebind backfill (ka-0000000462) is a follow-on task that
  rewrites historical `owner = slack:U0OPERATOR1` rows where Matt is
  not the spawn-chain root. It is **out of scope here** and lands as a
  separate MR after this one.
- The C2c resolver chain walk that would fully equate `slack:U0OPERATOR1`
  and `oidc:matt@example.com` via the operator-aliases graph is **out of
  scope here** (ka-0000001132). For now the mismatch check is a string
  compare on canonical identity; aliases are not collapsed.

## Alternatives considered

### A. Keep verbatim X-Operated-By default (status quo, ka-0000000269)

Rejected. This is exactly the disaster being undone. Any chain that
relays a header (i.e. all of them today) ends up attributing leaf work
to whoever is at the top of the spawn tree. Single point of failure for
ownership; collapses multi-user readiness.

### B. Spawn-chain-root-only with X-Owner-Override escape hatch (ka-0000000058)

Rejected by Matt in decision ka-0000003484. The chain walk on every
write is expensive (a store lookup per spawn-chain hop, up to the
walk cap), the override-only escape hatch forces every bridge that
*already* knows the right human via `X-Operated-By` to re-push it as
`X-Owner-Override`, and the common-case "header IS the right human" is
left empty even when the header and the chain agree. The right answer
is to *use* the header when it is correct, and only reject when it
disagrees with the chain — which is precisely keep-header-but-tighten.

### C. Unowned-default for everything (sentinel-only)

Rejected. Loses the bridge cases where `X-Operated-By` carries a
genuinely correct human (Slack `/spawn` from a non-Matt user,
jira-bridge mirroring a ticket reporter, OIDC UI create). Forces every
caller to push a separate override header just to recover ownership the
header already conveyed.

### D. Defer — wait for C2 (kill agents-as-operators) to land first

Rejected. C2 is a longer-running fix that rewrites the resolver to walk
to a human on every write surface. The Matt-as-default bleed is live
right now; we want the tightened policy to land first and stop the
bleed, with C2 layered on top later. The two changes are compatible:
C2 narrows when the audit operator is a human; this ADR narrows when
the *Owner* defaults from that human's id.

## Implementation pointers

1. `kbeads/internal/server/auto_owner.go`
   - `applyOwnerDefault(ctx, owner)`: implement the precedence ladder
     above. Read `operatedByHeaderFromContext(ctx)` for the value,
     re-check `isHumanOperator` (from audit.go), and for static-token
     agent callers (`id.Type == "static" && id.Subject != "static"`)
     compare against `resolveSpawnChainRoot(ctx)`. Emit
     `slog.Warn("owner_default_mismatch", …)` on rejection.
   - Keep `WithOwnerOverride`, `ownerOverrideFromContext`,
     `sanitizeOwnerOverrideHeader`, `WithNoOwnerDefault`,
     `noOwnerDefaultFromContext`, `parseNoOwnerDefaultHeader`,
     `ownerDefaultEnabled`, `resolveSpawnChainRoot`,
     `humanIdentityFromAgentBead`, `agentSpawnedByField`,
     `isSentinelSpawner`, and `canonicalIdentityFromUserBead`
     unchanged — they all still apply.
2. `kbeads/internal/server/beads.go` — the existing one-line hook
   `in.Owner = s.applyOwnerDefault(ctx, in.Owner)` in `createBead` is
   unchanged.
3. Tests in `kbeads/internal/server/auto_owner_test.go`:
   - Header + non-agent static caller (subject="static") → Owner = header.
   - Header + fleet/system/non-human shape → Owner empty.
   - Header + agent caller + chain-root match → Owner = header.
   - Header + agent caller + chain-root mismatch → Owner empty
     (the Matt-disaster shape).
   - `X-Owner-Override` still wins.
   - `-` sentinel and `X-No-Owner-Default` still work.

## Open questions

- The mismatch warning is structured `slog.Warn` only — should we also
  bump a Prometheus counter so dashboards can plot the rejection rate
  alongside `operated_by_*` metrics? Punted: file a follow-up if the
  rate stays material after C4 backfill.
- Should `X-Owner-Override` validate the value against the operators-
  aliases graph before accepting? Cheap-and-safe answer: yes, but
  punted to the C2c work since it's the same resolver path.
