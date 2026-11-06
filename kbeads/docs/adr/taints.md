# ADR: Taints — inheritable data-contamination markers on beads

**Status**: Proposed — design only, no code lands from this doc. Phase 1 (§14) landed 2026-09-20; §15 amended the same day.
**Date**: 2026-09-18
**Author**: laptop-claude, for the operator
**Bead**: none yet
**Supersedes**: nothing
**Depends on**:
- `docs/adr/human-attribution-identity-v2.md` (operators + aliases: the principal a taint is owned by and granted to)
- `docs/adr/owner-default.md` (the spawn-chain walk this design reuses)
- `gardenio/docs/research/obs-centralization-w2-pii.md` (hash-only audit sampler; the model for the scrub artifact)
**Related**:
- `internal/server/read_authz_scope.go` — the two-gate project read scope taints extend
- `internal/server/auto_inherit.go`, `auto_owner.go` — create-time inheritance hooks
- `internal/server/investigation_request.go`, `scif_approval_authz.go` — born-inert request pattern
- `internal/server/http_escalations.go`, `write_authz.go` — request → approve → replay primitive
- `internal/server/decision_aging.go` — precedent for policy-driven auto-approval

## TL;DR

Gardenio is going multi-tenant, multi-user, multi-team. Agents need broad access to
data and copy it freely between beads, comments, sessions, and derived stores. Once
production data is in the bead graph there is no way to say *which* beads carry it,
*who* may see them, or *how* the contamination is removed. This ADR adds **taints**:
named, server-owned markers that

1. attach to a bead and **propagate automatically** to the agent working it, that
   agent's session, and every bead the agent creates or writes to;
2. **gate reads**: a caller sees a tainted bead only if it holds a grant for every
   taint on it;
3. are removed from the bead they were attached to only through a scrub request,
   approved under a policy the taint's owner defines (by a human, by two humans, or
   automatically when the policy's predicate holds). `purge` destroys the content in
   place via a cascading store operation that reaches every copy, and *does* drop
   the now-pointless marker. `detaint` is policy-selectable between two modes
   (§6.1): the default, `fork`, forks an untainted successor bead from attested
   clean content while the tainted original is closed, not stripped, so the marker
   never leaves the original; the higher-risk `strip` mode removes the marker from
   the original in place, but only after the same content cascade `purge` runs,
   because the attested rewrite otherwise leaves the pre-image behind;
4. leave an audit artifact that is handed to the approver and never stored in
   cleartext;
5. are owned, granted and approved by **teams**, a new `type:team` config bead,
   with defaults that are inert when nothing is configured and strict when a taint
   is first created.

Taints are **not labels**. Labels are caller-writable and the project label that
already scopes reads today can be stripped by the bead's own assignee
(`handleAddLabel`/`RemoveLabel` run only the ownership gate; the gRPC twins run none).
Taints get their own tables and a write path closed to everyone except the taint
framework.

Scope is Gardenio (kbeads) only. Gas City is out of scope for this ADR.

## 1. Context

### 1.1 The problem

The operator's framing: **data is toxic waste**. An agent that reads a production
ticket will quote it in a comment, summarize it in a report bead, embed it in a
vector, and leave it in mutation history — none of which is tracked. Multi-tenancy
makes this fatal: two teams sharing a daemon cannot be allowed to have one team's
production data drift into the other's beads with no marker and no way back.

Three stories drive the design:

| # | Story | What it needs |
|---|---|---|
| 1 | An agent works a production JIRA ticket carrying `jira-prod` and `acme-prod`. The bead it is assigned, the agent's own bead, its session, and every bead it creates carry the same taints. | propagation on assignment, on create, on write |
| 2 | A developer agent needs to look at production data. It must hold an `acme-production` grant, and its beads become tainted. The taint owner has defined a scrub workflow the developer can run to detaint the developer's own beads — by default forking clean, untainted successors while the tainted originals stay closed and intact, or, where the owner's policy opts into the higher-risk `strip` mode, by stripping the marker from the originals in place after a full content cascade — possibly with an approval step by an approved principal. | read gating, taint acquisition on read, owner-defined scrub policy (including detaint mode), human-or-auto approval |
| 3 | An administrator scrubs every production-tainted bead older than 30 days and gets an audit artifact that lives only on the administrator's machine. | bulk purge by predicate, artifact returned not stored |

### 1.2 What exists today

- **Project scoping** (`read_authz_scope.go`): a project-bound fleet token's
  `project:<name>` label is ANDed into every list filter; an OIDC user's
  `allowed_projects` are ORed in. Same two gates on by-id reads. This is the shape
  taints extend — but keyed on a label anyone with ownership can rewrite.
- **Create-time inheritance** (`auto_inherit.go`, `auto_owner.go`): `operated_by`,
  `user_bead_id`, `slack_user_id` default from request context; `Owner` defaults
  from the `X-Operated-By` header after a spawn-chain walk that resolves the calling
  agent's bead from the static-token subject (`AuthIdentity.Subject` **is** the agent
  bead id). Taint propagation is the same hook with a different source.
- **Born-inert requests** (`investigation_request.go`): approval, launch, and result
  fields are rejected at create; `requested_by` is server-stamped; approval is
  written later by a named subject on an exact field allowlist
  (`scif_approval_authz.go`).
- **Escalation replay** (`write_authz.go` `handleEscalation`,
  `http_escalations.go`): a denied write plus `X-Escalate: true` stores the operation
  spec on an escalation bead; approve merges approval fields, audits, replays the op
  under `WithMachineOperator`, records the result, closes, and runs the close cascade.
  This is the general request → approve → privileged-execute primitive.
- **Policy auto-resolution** (`decision_aging.go`): stale decisions with a declared
  default are resolved as `system:decision-aging` through the same merge → close →
  publish → cascade path an operator answer takes.
- **Formula gates**: a formula step may be `gate: {type: decision|manual|sync}`;
  `gate_type` is stamped on the step bead and resolving the decision runs
  `afterBeadClosed`, which parks or releases gated work.
- **Hygiene**: credential-shape redaction on every prose write (`cred_redact.go`),
  soft delete with restore (`queryDeleteBead`, hard purge "future"), audit retention
  sweep, forget-operator soft delete with a large-history guard.

### 1.3 Where tainted content actually lives

A scrub that touches only the `beads` row is a lie. Verified copies of bead content:

| Store | Copy | Maintained by |
|---|---|---|
| `beads` | title, description, notes, fields | app |
| `beads.search_tsv` | generated from the above | trigger `beads_search_tsv_update` (039/040) — recomputes on UPDATE |
| `comments.text` | prose | app; `DeleteComment`, `TrimComments` exist |
| `events.payload` | **whole bead** on `BeadCreated`/`BeadUpdated`, whole comment on `CommentAdded` | app, append-only |
| `mutation_history.old_value/new_value` | field-level before/after | app, append-only |
| `bead_embeddings.embedding` | vector of the text | app; `DeleteEmbedding` exists |
| `bead_references` | extracted ids only | app — no content |
| `deps.metadata` | free text | app |
| `session_gates.metadata` | JSON | app |
| coop session recordings | terminal transcript | coop, redacted for credential shapes only |
| daemon logs / traces | fragments | out of process |

Everything above the double rule is in the daemon's own database and **must** be
cascaded by the scrub op. Recordings, logs, and traces are out of scope for this
ADR (§13, open question 4).

## 2. Decisions

| # | Decision | Choice |
|---|---|---|
| D1 | Storage | Dedicated `taints` registry + `bead_taints` join table + `taint_grants`. Not labels, not `fields`. |
| D2 | Propagation | **Union, never automatic removal.** On create: caller agent's taints ∪ parent's ∪ explicit source. On assignment and on read by an agent: the agent bead acquires the taint. On any write by a tainted agent: the target acquires the agent's taints. |
| D3 | Read gating | A caller may read a bead only if it holds a grant for **every** taint on it. Applied through the same list-filter and by-id gates as project scope, plus the event and graph aggregations. |
| D4 | Scrub | A `scrub-request` bead born inert, approved under the taint owner's policy, executed by a new `scrub` escalation-replay operation. `purge` runs a cascading store op that destroys content in place. `detaint` is policy-selectable between two modes (§6.1, decision gd-Swd7mnGf3L): `fork` (default) forks an untainted successor from attested clean content and closes the original, which keeps its taint forever; `strip` removes the marker from the original in place, but only after cascading the pre-attestation content the same way `purge` does. Workflow around any of these is a formula. |
| D5 | Where | kbeads. The daemon is the only component that sees every write. |
| D6 | Auto-approval | The taint owner's scrub policy maps predicates to modes `auto`, `human`, `human-four-eyes`. `auto` is approved by a server-side evaluator as `system:taint-policy`, with per-request and per-taint blast-radius caps. |
| D7 | Teams | A `type:team` config bead is the principal that owns taints, holds grants, and approves scrubs. Membership is explicit ids or IdP groups. Agents are never members. |
| D8 | Defaults | Inert with nothing configured; a new taint is `human-four-eyes` for purge and `human` attested for detaint with the owner team as approvers; presets for one-command setup; per-project intake taints. |
| D9 | Effects | A taint carries a **set** of effects, `taints.effects`, default `{gate}`. Only `gate` is implemented; `restrict`, `evict`, `sink` are reserved with the semantics in §15.2. The empty set is legal and means trace-only. |
| D10 | Grants are tolerations | A grant is the toleration for a taint, and what it tolerates is defined per effect (§15.3). Under `gate` a granted agent still acquires the taint on read; under `restrict` a granted principal is a trusted subject that reads without acquiring. |

## 3. Schema

All DDL idempotent per the kbeads migration policy; no eager backfill (there is
nothing to backfill — a fresh install has no taints).

```sql
-- One row per named taint. The name is an alias; the id is what rows key on, so
-- a rename never orphans anything.
CREATE TABLE IF NOT EXISTS taints (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,          -- e.g. 'acme-production', 'jira-prod'
    description   TEXT NOT NULL DEFAULT '',
    owner_team    TEXT NOT NULL,                 -- type:team bead id (§8)
    scrub_policy  JSONB NOT NULL DEFAULT '{}',   -- §6.2
    effects       TEXT[] NOT NULL DEFAULT '{gate}',  -- §15.2; reserved 2026-09-20, only 'gate' is enforced
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at    TIMESTAMPTZ                    -- retired taints still gate reads; they just cannot be newly applied
);

-- Which beads carry which taints, and how they got there.
CREATE TABLE IF NOT EXISTS bead_taints (
    bead_id        TEXT   NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    taint_id       BIGINT NOT NULL REFERENCES taints(id),
    source         TEXT   NOT NULL,   -- 'explicit' | 'inherit:create' | 'inherit:assign' | 'inherit:read' | 'inherit:write' | 'inherit:parent'
    source_bead_id TEXT,              -- the bead or agent bead the taint flowed from
    applied_by     TEXT   NOT NULL,   -- server-derived subject, never client-supplied
    applied_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (bead_id, taint_id)
);
CREATE INDEX IF NOT EXISTS idx_bead_taints_taint ON bead_taints(taint_id);

-- Who may read beads carrying a taint. principal_kind ∈ {'team','operator','agent','fleet'}.
CREATE TABLE IF NOT EXISTS taint_grants (
    taint_id        BIGINT NOT NULL REFERENCES taints(id),
    principal_kind  TEXT   NOT NULL,
    principal       TEXT   NOT NULL,   -- team bead id, operators.id as text, agent bead id, or fleet name
    granted_by      BIGINT NOT NULL REFERENCES operators(id),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,       -- agent grants are always bounded (§5.2)
    PRIMARY KEY (taint_id, principal_kind, principal)
);

-- Hash-only record of every EXPLICIT taint mutation. Never holds content.
-- Pinned 2026-09-20: rows are written for grant, revoke, explicit apply and the
-- scrub-* actions. Propagation (bead_taints.source = inherit:*) is NOT audited
-- here: it is the highest-volume event and the bead_taints row already carries
-- its own provenance (source, source_bead_id, applied_by, applied_at). One store
-- method writes bead_taints for both explicit apply and propagation (ApplyTaint,
-- returning inserted so an explicit no-op re-apply writes no audit row).
CREATE TABLE IF NOT EXISTS taint_audit (
    id            BIGSERIAL PRIMARY KEY,
    taint_id      BIGINT NOT NULL REFERENCES taints(id),
    action        TEXT   NOT NULL,   -- 'apply' | 'grant' | 'revoke' | 'scrub-request' | 'scrub-approve' | 'scrub-execute' | 'scrub-reject'
    bead_id       TEXT,
    request_id    TEXT,              -- the scrub-request bead
    actor         TEXT   NOT NULL,   -- server-derived
    approval_mode TEXT,              -- 'auto' | 'human' | 'human-four-eyes'
    content_sha   TEXT,              -- sha256 of the content that was removed, when applicable
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

`taints.scrub_policy` shape (validated at `SetTaint` time, rejected if malformed):

```json
{
  "detaint_mode": "fork",
  "rules": [
    {"when": {"older_than_days": 30, "kind": "purge"},              "mode": "auto"},
    {"when": {"requester_is_chain_root": true, "kind": "detaint",
              "attested": true},                                    "mode": "auto",
              "max_beads": 50, "max_per_day": 200},
    {"when": {"kind": "detaint"},                                   "mode": "human",
              "approvers": ["team:acme-prod-owners"]},
    {"when": {"kind": "purge"},                                     "mode": "human-four-eyes",
              "approvers": ["team:acme-prod-owners"]}
  ],
  "default": "human-four-eyes"
}
```

Rules are evaluated first-match against approval `mode` (`auto`/`human`/
`human-four-eyes`) — a separate axis from `detaint_mode`. No rule matching falls to
`default`, which may not be `auto`. `detaint_mode` (`fork` | `strip`, default `fork`
when omitted, §6.1) picks how *any* matched `detaint` request is executed once
approved; it does not affect which rule matches or which approval mode is chosen.
`strip` is the higher-risk mode — it rewrites the same bead's content in place
rather than leaving the tainted original untouched — so a policy that sets it
should pair it with `mode: human` or stricter for `kind: detaint` rules, never
`auto` alongside a low approval bar.

## 4. Propagation

Taint flows with information. Reads contaminate the reader's session; writes
contaminate the target. Removal is never implicit.

### 4.1 The calling agent

An agent's static token subject is its agent bead id (`resolveSpawnChainRoot`
already does `GetBead(ctx, id.Subject)`). The agent bead is the **session carrier**:
the taints on it are the taints of the session. `session_gates` already hangs off
`agent_bead_id`, so nothing new is needed to model "the session".

A **taint context** is resolved once per request from `AuthIdentityFromContext`:

- static token with an agent subject → taints of that agent bead;
- OIDC human, primary static token, fleet token → empty (principals do not carry
  taints; they hold grants).

### 4.2 Rules

| Event | Effect | `source` |
|---|---|---|
| `createBead` | new bead gets caller-agent taints ∪ parent bead taints ∪ taints on any bead named in `triggered-by`/`derived-from` deps ∪ its project's `intake_taints` (§10.3) | `inherit:create`, `inherit:parent`, `inherit:project-intake` |
| assignment (`assignee` set to an agent, or an agent bead created with a task) | agent bead gets the task's taints | `inherit:assign` |
| any read of a tainted bead by an agent | agent bead gets the bead's taints | `inherit:read` |
| any write to a bead by a tainted agent (update, comment, label, dep, close) | target bead gets the agent's taints | `inherit:write` |
| explicit `kd taint apply <bead> <taint>` by a grant holder | bead gets the taint | `explicit` |
| formula pour / molecule expansion | children get the molecule's taints | `inherit:parent` |
| escalation replay of a create/update | replayed write inherits from the **requesting** agent, not the approver | `inherit:create` for create, `inherit:write` for update |

`inherit:read` is the rule that makes story 2 true and is the one with a cost: a read
now performs an idempotent `INSERT … ON CONFLICT DO NOTHING` when the agent lacks the
taint. The per-agent taint set is tiny and changes rarely, so the taint context is
cached per agent bead for the `activeAgentCacheTTL` window and the insert fires only
on a miss.

The hook lives in `createBead` between `applyAutoInherit` and `authorizeWrite`, and in
`updateBead`/comment/label/dep handlers after `loadBeadForWrite`. `X-No-Auto-Inherit`
does **not** disable taint propagation. There is no opt-out header.

### 4.3 Events, mutation history, embeddings

These tables are keyed by `bead_id` and carry no taint columns. Read scope for them
joins through `bead_taints` the same way `eventReadScope` joins through labels
today. Embedding search results are filtered post-query by the same predicate before
they leave the server.

## 5. Read gating

### 5.1 The gate

`beadReadAuthorized` gains a third check: `taintsOf(bead) ⊆ grantsOf(caller)`.
`scopeListFilter` gains a `NOT EXISTS (SELECT 1 FROM bead_taints bt WHERE
bt.bead_id = b.id AND bt.taint_id NOT IN (<caller grants>))` predicate. Both the
project gates and this gate narrow; nothing widens.

The gate covers every read that returns a bead's text, not only `GET /v1/beads/{id}`
and lists (gd-DbxpPLBcsi):

- the per-bead sub-resources (`comments`, `labels`, `dependencies`, `dependents`,
  `events`, `mutations`, `operator-history`, `followers`, `taints`, `mentions`,
  `mentioned-by`, and their gRPC twins), through `subresourceParentReadable`: a denied
  parent answers as a missing one;
- the by-id reads that fetch a bead through `authorizeBeadRead` (`GET
  /v1/decisions/{id}`, `/v1/escalations/{id}`, `/v1/releases/{id}`), which answer 404;
- the rows of `mentions` and `mentioned-by`, which carry the other bead's title: a
  `mentions` target the caller may not read is blanked to a dangling reference, and a
  `mentioned-by` source it may not read is dropped.

Grants of a caller:

- OIDC human → `taint_grants` rows for their `operators.id`, plus rows for every
  team they belong to (§8.1), plus implicit ownership of taints their teams own;
- agent → rows for the agent bead id, plus rows for the spawn-chain root human
  (an agent may read what the human who started it may read), bounded by
  `expires_at`;
- fleet token → rows for the fleet name;
- primary static token (`subject == "static"`) → see §13 open question 1.

### 5.2 Agent grants are bounded and derived

An agent bead is granted a taint automatically when it is assigned a bead carrying
that taint **and** the assigning principal holds the grant. The grant's `expires_at`
is the agent bead's close, enforced by the close cascade revoking it. A human cannot
grant an agent a taint the human does not hold.

### 5.3 What a denial looks like

A read that fails the taint gate returns 404, not 403, on by-id reads (the existence
of a tainted bead is itself information) and is silently absent from lists. The
denial is counted in `authz_denials_metrics` under a `taint` reason.

## 6. Scrub

### 6.1 Two kinds, and two modes for one of them

Revised 2026-09-20 per decision gd-Swd7mnGf3L (D4): whether a taint may ever leave
the bead it was attached to is not one global answer — it is chosen per taint by
that taint's owner, through `scrub_policy.detaint_mode` (§3). The two kinds still
diverge on whether content is destroyed, but `detaint` now carries two modes behind
that switch:

- **`detaint`** — the content is deemed clean. What happens to the original depends
  on `scrub_policy.detaint_mode` (default `fork`):
  - **`fork`** (default) — the **original bead keeps its taint forever**. A
    `CreateBead` forks a new, untainted successor from the attested clean content
    (§6.4), and the original is closed (`close_reason: scrubbed`) and linked to the
    successor via a `supersedes` dep edge — so the tainted history stays readable
    (to anyone still holding the grant) rather than disappearing. Neither the
    original's content nor its `bead_taints` row is ever touched.
  - **`strip`** (higher-risk, opt-in) — the marker is removed from the **same**
    bead, in place. Because the attested rewrite would otherwise leave the
    pre-attestation production content sitting in `mutation_history`, `comments`,
    `bead_embeddings` and `events` (§1.3), `strip` is not a cheaper `fork` — it
    must run the **same content cascade `purge` runs** (§6.5) before the marker
    row is dropped. Pick `strip` only when the bead identity itself, not just its
    content, must survive the scrub — e.g. an external system holds a durable
    reference to this exact bead id.
  Requires an attestation (§6.4) unless the policy says otherwise. Story 2.
- **`purge`** — destroy the content in place, then remove the marker. This is the
  only kind with no owner-configurable mode: it always ends with the taint actually
  gone, because there is no longer any content left for it to protect. Story 3.

### 6.2 The request

A new data-kind bead type `scrub-request`, born inert exactly like
`investigation-request`:

| Field | Set by |
|---|---|
| `taint` (name) | requester |
| `kind` (`detaint`/`purge`) | requester |
| `scope` (`{"bead_ids": [...]}` or `{"older_than_days": 30, "labels_any": [...]}`) | requester |
| `requested_by` | **server-stamped** from `actorFromContext` |
| `resolved_bead_ids` | server, at create: the scope evaluated once and frozen |
| `attestation` | attesting agent, via the formula step (§6.4) |
| `approval_status`, `approved_by`, `approved_at`, `approval_mode`, `approval_reason` | **forbidden at create**; written by the approve path only |
| `executed_at`, `result`, `artifact_sha` | **forbidden at create**; written by the replay only |

The requester must hold a grant for the taint. A `purge` request whose scope includes
a bead the requester cannot read is rejected at create — you cannot destroy what you
cannot see.

### 6.3 Approval

`POST /v1/scrub-requests/{id}/approve` reuses `handleApproveEscalation`'s shape with
three corrections that this ADR makes mandatory:

1. **Approver is the authenticated identity**, via `decisionAttribution`-style
   derivation. A body-supplied `approved_by` is logged and ignored (today's
   escalation approve takes it from the body).
2. **OIDC only.** `requireRole` short-circuits for non-OIDC callers; the scrub
   approve handler additionally refuses any non-OIDC identity outright, so no
   static or fleet token can approve — **including** one relaying a human
   identity via `X-Operated-By`, the path `handleApproveEscalation` accepts
   for the beads-viewer relay. That relay is not a plausible approval channel
   for a destructive scrub: any holder of the shared static token controls
   the header value it would relay, which would let it name an arbitrary
   approver. The caller must have authenticated the OIDC session itself.
   `edge` (gardenio-edge-auth, ka-0000001641) is treated as OIDC-equivalent
   for escalation approve but is **not** accepted here absent a future,
   deliberate amendment to this section.
3. **Four-eyes** when the mode is `human-four-eyes`: `approved_by ≠ requested_by`
   after alias resolution through `operator_aliases`, and `approved_by` must match
   the rule's `approvers`.

The **policy evaluator** runs at request create and re-runs at approve:

- pick the first matching rule from `taints.scrub_policy`;
- `auto` → approve immediately as `system:taint-policy` (mirroring
  `autoDefaultDecision`), subject to `max_beads` on this request and `max_per_day`
  across the taint; over cap falls through to the next rule;
- `human`/`human-four-eyes` → leave pending, create a `decision` bead the formula
  gate parks on, and notify the approver group.

The evaluator is the only thing that may write `approval_mode: auto`. An agent
cannot request auto; it can only file a request that a rule happens to auto-approve.

### 6.4 Attestation

For `detaint` (either mode), the policy may require `attested: true`. The scrub
formula (§6.6) includes an agent step that reads every bead in scope, rewrites or
strips production content, and writes it back onto the **scrub-request** — never
onto the original bead, which review must not mutate, regardless of mode:

```example
attestation: {
  agent_bead_id,
  content_shas:  {bead_id: sha256(original content at review time)},
  clean_content: {bead_id: {title, description, notes, fields}},
}
```

`content_shas` is the drift check: the evaluator checks that every
`resolved_bead_ids` entry has a matching `content_shas` entry whose hash equals the
bead's **current** content hash — a bead edited after attestation fails closed.
`clean_content` is what execution (§6.5) writes, but *where* it lands depends on the
mode: `fork` uses it to `CreateBead` the untainted successor (that bead doesn't
exist yet, so it is never hash-checked, only staged); `strip` uses it to overwrite
the original's own `title`/`description`/`notes`/`fields` as the first step of the
same-transaction cascade, immediately before the content-bearing tables are blanked
(§6.5) — so the original is never left, even momentarily, both tainted and holding
the raw pre-image. The attesting agent reads tainted data and therefore acquires
the taint itself (`inherit:read`); that is correct, not a leak — for `fork` the
taint stays on the attesting agent and the original, never on the successor it
creates; for `strip` it stays on the attesting agent, and on the original only
until the strip completes and removes it there too.

### 6.5 Execution

A new escalation-replay `operation_type: "scrub"` runs under `WithMachineOperator`
and calls one store method, `ScrubTaint(ctx, req)`, in a single transaction. `kind`
picks the top-level branch; for `detaint`, the taint's `scrub_policy.detaint_mode`
(§3) picks the second one. `purge` and `detaint`/`strip` share the same content
cascade over every in-daemon copy; `detaint`/`fork` is the only path that never
touches the original's content or its `bead_taints` row at all:

```example
for each bead in resolved_bead_ids:
    if kind == purge:
        beads:            title = '[purged]', description = '', notes = '', fields = fields − prose keys   (search_tsv recomputes via trigger)
        comments:         text = '[purged]'   (rows kept so ids and authorship survive)
        events:           payload = jsonb_strip_content(payload)   -- keeps topic, ids, actor; drops bead/comment bodies
        mutation_history: old_value = '', new_value = '' where field ∈ prose fields
        bead_embeddings:  DELETE
        deps.metadata, session_gates.metadata: '' for rows referencing the bead
        bead_taints:      DELETE where taint_id = req.taint   -- nothing sensitive is left for it to protect
        taint_audit:      INSERT (action='scrub-execute', content_sha=sha256(pre-image))

    if kind == detaint and mode == fork:
        -- the original's content and bead_taints row are NOT touched, at all:
        beads:            status = 'closed', fields.close_reason = 'scrubbed'
        taint_audit:      INSERT (action='scrub-execute', content_sha=sha256(current content))
        successor = CreateBead(attestation.clean_content[bead.id])   -- runs as
                                                                      -- WithMachineOperator, carries
                                                                      -- no taints, so ordinary
                                                                      -- create-time propagation does
                                                                      -- not re-taint it
        deps:             INSERT {bead_id: successor.id, depends_on_id: bead.id, type: 'supersedes'}
        -- no other table is touched for the original: no rewriting of deps,
        -- bead_references, or comments that point at it — existing blocks/
        -- blocked-by edges into it clear the moment it closes, same as any
        -- other close

    if kind == detaint and mode == strip:
        -- same-bead rewrite, so the pre-attestation pre-image must not survive
        -- anywhere: run purge's full cascade on THIS bead, but land the attested
        -- clean content instead of a '[purged]' placeholder:
        beads:            title/description/notes/fields = attestation.clean_content[bead.id]   (search_tsv recomputes via trigger)
        comments:         text = '[purged]'
        events:           payload = jsonb_strip_content(payload)
        mutation_history: old_value = '', new_value = '' where field ∈ prose fields
        bead_embeddings:  DELETE   -- recomputed on the next write, against the clean content
        deps.metadata, session_gates.metadata: '' for rows referencing the bead
        bead_taints:      DELETE where taint_id = req.taint   -- clean content and marker both gone, same bead
        taint_audit:      INSERT (action='scrub-execute', content_sha=sha256(pre-image))
```

`purge` is **not** `DeleteBead`. Soft delete keeps every copy; the point is to destroy
the copies while keeping the graph shape (ids, edges, authorship, timestamps) so
nothing downstream dangles. `detaint`/`strip` reuses that same cascade for the same
reason, on top of landing the attested clean content rather than a `[purged]`
placeholder. `detaint`/`fork` is not `DeleteBead` either, in the opposite direction:
the original is closed, not deleted, so its full tainted content stays readable (to
anyone who still holds the grant) via the `supersedes` edge — scrubbing a bead gives
you a clean copy to keep working from, not an admission that the tainted one never
existed. The undo path (`undo.go`) refuses mutations produced by a scrub: the
`mutation_history` rows it writes (the purge/strip content-blanking, or the fork
close + `close_reason` change) carry `actor = system:taint-scrub`, which
`undoMutation` treats as `ErrUndoUnsupportedMutation`.

### 6.6 The workflow is a formula

The taint owner authors a formula, e.g. `scrub-acme-production-detaint`:

```example
steps:
  - id: review     # agent task: read scope, strip prod data, write attestation
  - id: approve    # gate: {type: decision}  — omitted when the policy rule is auto
    waits_for: [review]
  - id: execute    # creates the scrub-request (if not already) and calls approve
    waits_for: [approve]
```

The formula's `taint` variable and the requester's grant are checked at pour. "How
general" is answered by the formula DSL; "what actually happens to the bead — fork
a clean successor, strip it in place, or destroy the content" is answered by the
replay op reading `kind` and, for `detaint`, the taint's `scrub_policy.detaint_mode`
(§6.1) — the formula itself is identical either way, since the mode is a property
of the taint's policy, not of the formula or the request. Story 3 is the same
formula with `kind: purge`, an `older_than_days` scope, no review step, and an
`auto` rule.

### 6.7 The artifact

The approve response body carries the audit artifact: request id, taint, kind,
approver, mode, and per-bead `{id, content_sha, tables_touched}`. The daemon stores
only `taint_audit` rows (hashes, ids, actors). `kd scrub approve` writes the artifact
to a local file the caller names. This satisfies "persisted only on the
administrator's machine" without a second storage system.

## 7. CLI and API surface

| Command | Endpoint | Role |
|---|---|---|
| `kd taint create <name> --owner <op> --policy <file>` | `POST /v1/taints` | admin |
| `kd taint list` / `kd taint describe <name>` | `GET /v1/taints[/{name}]` | viewer (names only; policies visible to owner/admin) |
| `kd taint grant <name> <principal> [--until]` | `POST /v1/taints/{name}/grants` | taint owner or admin |
| `kd taint revoke …` | `DELETE …` | taint owner or admin |
| `kd taint apply <bead> <name>` | `POST /v1/beads/{id}/taints` | grant holder |
| `kd taint show <bead>` | `GET /v1/beads/{id}/taints` | reader of the bead |

Command surface, pinned 2026-09-20 after two crews claimed `show` for different
things: **`show` takes a bead id and answers "what taints are on this bead and
why"**, the everyday developer question (§9); **`describe` takes a taint name and
answers "what is this taint"** (definition, owner, policy where visible). `list`
is definitions only. Nothing else under `kd taint` uses `show` or `describe`.
| `kd scrub request --taint … --kind … --scope …` | `POST /v1/scrub-requests` | grant holder |
| `kd scrub approve <id> --artifact <path>` | `POST /v1/scrub-requests/{id}/approve` | OIDC human in the rule's approvers |
| `kd scrub reject <id> --reason` | `POST …/reject` | same |

There is **no** `DELETE /v1/beads/{id}/taints/{name}`. The only removal path is a
scrub request. The gRPC service gets no taint RPCs (see §13, open question 2).

## 8. Teams

Taints need a principal that is not one human and not a whole fleet: the group that
owns production, the team that may approve a purge, the people who hold a grant
because of the job they do rather than who they are. Nothing in kbeads models that
today. Operators are individuals; fleets are infrastructure; projects are code
scopes; OIDC groups arrive on a token but are never stored or displayed.

### 8.1 Decision

A **team** is a config-kind bead, `type:team`, in the same family as `type:project`
and `type:account`: strict schema, admin-only writes, visible in the viewer for free,
cross-fleet like project and account. It is the principal kind that taints are
owned by, that grants are given to, and that scrub policies name as approvers.

```json
{
  "kind": "config",
  "write_policy": {"min_role": "gardenio-admin"},
  "fields": [
    {"name": "team",        "type": "string", "required": true},
    {"name": "display",     "type": "string"},
    {"name": "members",     "type": "json"},
    {"name": "oidc_groups", "type": "json"},
    {"name": "owners",      "type": "json"},
    {"name": "projects",    "type": "json"}
  ]
}
```

- `members` is a list of operator ids (or emails, resolved through
  `operator_aliases` at write time and stored as ids).
- `oidc_groups` is a list of IdP group names. When the caller's OIDC token carries
  one of them, the caller is a member for this request even if `members` does not
  list them. This is how the team follows the IdP once there is one, and how it
  works with explicit members until then.
- `owners` is the subset of members who may edit the team's own membership without
  being platform admins, and who may act as the team when a policy names it as
  approver. Empty means every member may.
- `projects` is informational for the viewer and for defaults (§10); it grants
  nothing by itself.

Membership resolution is one function, `teamsOf(ctx) []teamID`, evaluated once per
request from the authenticated identity: explicit `members` containing the
caller's operator id, or `oidc_groups` intersecting the token's groups. Agents are
never team members. An agent acts under its spawn-chain root human's teams for
grant resolution (§5.1) and under nobody's teams for approval.

### 8.2 What changes elsewhere

- `taints.owner_team` replaces `owner_id`. A taint is owned by a team. The CLI
  creates a one-person team on the fly for a solo operator (`team:<slug>`), so the
  model never has a special case for "owned by a human".
- `taint_grants.principal_kind` gains `team`. Grant resolution unions the caller's
  own rows, their teams' rows, and (for agents) the chain-root human's rows and
  teams' rows.
- Scrub policy `approvers` name teams. **Stored form is `team:<team bead id>`**
  (for example `team:gd-ORaCUV7C6g`), the same identifier `taint_grants.principal`
  stores and `teamsOf` returns, so the evaluator compares ids and never resolves
  names. The CLI and viewer accept `team:<slug>` (the team bead's `team` field),
  resolve it to the bead id before the write, and refuse an ambiguous or unknown
  slug. Examples in this document use slugs for readability only. The bare
  `group:` form from the earlier draft is dropped; an IdP group is reached by
  putting it in a team's `oidc_groups`. (Pinned 2026-09-20 while reviewing the
  store methods: `DefaultScrubPolicy` already writes the bead-id form.)
- Four-eyes is evaluated on operators, not teams: two members of the same team may
  be requester and approver, but they must be two different people after alias
  resolution.

### 8.3 Why a bead and not a table

Project, account, fleet and user are all beads. A team as a bead gets the viewer
page, the event history, the `kd` CRUD, mutation history and the strict write
policy without new plumbing, and `crossFleetTypes` makes it visible to every fleet
like project and account. The cost is that membership checks read a bead's JSON
rather than a join; the set is small and is cached per request. If teams ever gate
something on the hot list path they can be materialised into a table without
changing the API.

## 9. Admin UX

Four personas touch taints. The design below names the surface each one uses and
what they see there, in the viewer, in `kd`, and in the Slack bridge.

| Persona | Does | Where |
|---|---|---|
| Platform admin | creates teams and taints, sets owners, retires taints, reads the global audit | viewer `/admin/taints`, `kd taint`, `kd team` |
| Taint owner (team) | edits the scrub policy, grants and revokes, approves scrubs, reads the taint's audit | viewer `/taints/{name}`, inbox, `kd scrub approve` |
| Developer / operator | sees which of their beads are tainted and why, requests a grant, requests a scrub, downloads their artifact | bead page badges, `/me/taints`, `kd taint show`, `kd scrub request` |
| Agent | no UI; gets a structured denial with advisory text and an escalation path | 403/404 body, `X-Escalate` |

### 9.1 Viewer

**Registry — `/taints`.** One row per taint: name, owner team, bead count, open
scrub requests, last scrub, retired flag. Admins see a "New taint" action that asks
for name, owner team and a preset (§10.2). Everyone with viewer role sees the list;
policies and grants are visible only to the owner team and admins. Bead counts are
computed under the viewer's own read scope, so a user who cannot read a taint's
beads sees the row but a dash for the count.

**Taint detail — `/taints/{name}`.** Tabs:

- *Policy*: the rules table rendered from `scrub_policy`, editable by owners as a
  form (kind, predicate, mode, approvers, caps), with the JSON behind a disclosure.
  Saving validates server-side; a rule with `mode: auto` and no cap is refused.
- *Grants*: who holds it (team, operator, fleet, agent), granted by whom, expiry;
  grant and revoke actions for owners; agent grants shown greyed with their
  spawn-chain root human.
- *Beads*: the tainted beads the viewer may read, with the propagation source
  column (explicit, create, assign, read, write, parent) and the source bead link.
  This is the "where did it spread" view.
- *Scrubs*: open and past scrub requests: kind, scope size, requester, mode,
  approver, state, with the approve and reject actions for approvers.
- *Audit*: the `taint_audit` rows for this taint, filterable by action and actor,
  hashes shown truncated. No content is ever rendered here because none is stored.

**Bead page.** A taint badge per taint in the header, coloured by the taint's
owner-team accent. Hover or tap shows the chain: "acme-production, inherited on
write from agent gardenio-crew-x5k, which acquired it reading gd-abc". Two actions:
"Request scrub" (pre-fills a detaint request for this bead) and, for owners,
"Apply taint". A bead the viewer cannot read because of a taint is simply absent;
there is no "you cannot see this" placeholder, per §5.3.

**Inbox.** A pending scrub request that needs a human is a `decision` bead
addressed to the approver team, so it lands in the existing decision queue with no
new inbox plumbing. The card shows kind, taint, scope size, requester, whether
attestation passed, the four-eyes state, and approve / reject with a reason. On
approve the viewer offers the artifact as a download and never stores it.

**Team page — `/teams/{team}`.** Members and owners, the IdP groups it follows,
taints it owns, grants it holds, its open approvals. Membership edits for owners
and admins.

**My taints — `/me/taints`.** For a developer: every taint on any bead they own or
created, grouped by taint, with a one-click "request detaint for all of mine" that
files one scrub request per taint scoped to those beads. This is story 2's entry
point.

### 9.2 CLI

`kd taint` and `kd scrub` as in §7, plus `kd team create | add | remove | show |
list`. Two additions that fall out of the UX:

- `kd taint grant-request <name> [--reason]` files a grant request to the owner
  team (an escalation bead, §9.4) so a developer does not need to know who owns
  the taint.
- `kd scrub approve` prints the artifact to stdout when `--artifact` is omitted, so
  it can be piped into whatever the administrator uses to keep it.

Every `kd` denial caused by a taint prints the same advisory text the API returns
(§9.4), never a bare 404.

### 9.3 Slack

The bridge posts to the owner team's channel (a new optional `slack_channel` field
on `type:team`) when: a scrub request needs their approval, a grant request is
filed, an auto-approved scrub executed, or a taint's per-day cap was hit. Approve
and reject stay in the viewer; Slack carries the link, not the action, so the
approval identity is always an OIDC session.

### 9.4 Denials and escalation

A write blocked by a taint returns the structured `AuthzDenial` body with
`denied_reason: taint`, the taint names, the owner team, and advisory text:

```example
Write denied: update on gd-abc carries taint "acme-production" that you do not hold.
Ask team acme-prod-owners for a grant, or retry with --escalate to file a grant request.
```

A read blocked by a taint returns 404 with no body change (§5.3); `kd` recognises
the case only when the caller asked about a bead id it was just given by another
call, and then prints "not found or not visible to you".

`X-Escalate: true` on a taint denial creates an escalation bead of
`operation_type: grant` addressed to the owner team. Approving it (same handler,
same three corrections) writes a `taint_grants` row with the expiry the approver
chooses, default 7 days. This reuses the escalation replay path rather than adding
a separate grant-request type.

The grant goes only to the filer: creating a `grant` escalation is refused unless
its `operation_spec` principal is the caller's own (the principal
`kd taint grant-request` resolves), and the replay refuses any principal that is not
the recorded requester: an agent other than the credential-verified
`requesting_agent`, a fleet or operator on an agent-filed escalation, or a team. The approver
must be one who could grant the taint directly, an admin or a member of its owner
team, or the replay is refused (gd-URd8GBIXvL).

## 10. Defaults

The design must be safe with nothing configured and useful with one command.

### 10.1 Zero configuration

| Situation | Behaviour |
|---|---|
| No taints exist | Nothing changes. No table is read on the hot path; the gate short-circuits on an empty registry cached per process. |
| A taint exists but no grants | Only the owner team can read tainted beads. Creating a taint grants the owner team implicitly (it is not a row; ownership implies the grant). |
| A taint exists with no policy | `{"default": "human-four-eyes"}` with the owner team as approvers. No rule is `auto`. Nothing is ever purged without two humans until someone writes a rule. |
| An agent is assigned tainted work | It is granted for the taint until its agent bead closes. Nobody has to grant agents by hand. |
| A bridge (jira, gitlab) creates a bead | No taint, unless the project bead declares `intake_taints` (§10.3). |
| Primary static token | Gated like any other principal, no bypass. (Was grant-all during the migration window via env; the escape was removed in phase 3, §13 OQ1.) |
| Denials | 404 on read, structured 403 with advisory text on write, both counted. |

### 10.2 Presets for `kd taint create --preset`

| Preset | Detaint | Purge | Auto rules | Meant for |
|---|---|---|---|---|
| `production` (default) | human, attested | human-four-eyes | none | customer or production data |
| `internal` | auto when attested and requester is chain root, cap 50 / 200 per day; else human | human | purge after 180 days | internal-only data that must not leave the team |
| `scratch` | auto when attested | auto after 7 days | as stated | experiments, demos, load tests |

A preset writes an ordinary `scrub_policy`; it is a starting point the owner edits,
not a mode the system remembers.

### 10.3 Per-project intake

`type:project` gains an optional `intake_taints: ["jira-prod"]`. Every bead the
bridges create from that project's sources, and every task spawned under it,
carries those taints at create. This is how story 1's ticket arrives tainted
without anyone touching it, and it is the only place a taint is applied by
configuration rather than by a principal or by propagation.

### 10.4 Grant expiry

| Grant to | Default expiry |
|---|---|
| team | none |
| operator | 90 days, renewable by the owner |
| fleet | none |
| agent | close of the agent bead |
| escalation-approved grant | 7 days, chosen by the approver |

### 10.5 Caps

An `auto` rule without `max_beads` is refused at policy write. A missing
`max_per_day` defaults to `10 × max_beads`. The evaluator counts from `taint_audit`,
never from client state.

## 11. Security invariants

1. **No client-supplied identity gates anything.** `applied_by`, `requested_by`,
   `approved_by`, `actor` are all derived from `AuthIdentityFromContext`.
   `X-Operated-By` and `X-Owner-Override` are never consulted.
2. **Taint rows are written only by the framework**: propagation hooks, explicit
   apply by a grant holder, and `ScrubTaint`. No generic bead update, field patch,
   label op, or admin backfill touches `bead_taints`.
3. **Grants never widen by inference.** An agent's effective grants are its own rows
   plus its spawn-chain root human's rows; a human's are their own plus their groups'.
   Nothing derives from `assignee`, `owner`, or `created_by`.
4. **A request is born inert.** Approval and execution fields are forbidden at create
   (`investigationRequestCreateForbiddenFields` pattern, own list).
5. **Auto-approval is policy-only, capped, and attributable.** Only the evaluator
   writes `approval_mode: auto`; `max_beads` and `max_per_day` are enforced at
   approve time from `taint_audit` counts, not from client state.
6. **Attestation is verified against current content.** A hash mismatch fails
   closed.
7. **`purge` and `detaint`/`strip` are a transaction over every in-daemon copy**
   (§1.3). A new table that stores bead content must be added to `ScrubTaint`'s
   cascade in the same MR, enforced by a test that diffs `information_schema`
   against the cascade list — the list is shared code between the two, not
   duplicated. `detaint`/`fork` (the default mode) is exempt from this cascade by
   design — it touches only the original's `status`/`close_reason` and creates a
   successor; it does not rewrite or blank any content-bearing table, because the
   original's content is never destroyed. `detaint`/`strip` is not exempt: because
   it rewrites the original's own content, it must run the same cascade `purge`
   does before it drops the marker, or the pre-attestation pre-image survives in
   `mutation_history`, `comments`, `bead_embeddings` and `events`.
8. **Retiring a taint does not lift it.** `retired_at` blocks new applies; existing
   rows still gate reads until scrubbed.
9. **Denials do not leak existence.** By-id taint denials are 404.

## 12. Alternatives considered

**A. Labels with a reserved prefix (`taint:<name>`).** Rejected. Labels are
caller-writable under the ownership gate on HTTP and unguarded on gRPC; the tainted
party could untaint itself. Adding a protected-prefix guard to every label path is
the same amount of work with a worse invariant.

**B. A JSON field on the bead (`fields.taints`).** Rejected. `fields` is patched by
agents constantly, `allow_additional_fields` is on for `type:agent`, and the
mutation-history/undo machinery would happily reverse a scrub.

**C. Per-taint encryption of content.** Considered for a later phase. It would make
`purge` a key deletion rather than a cascade, which is attractive, but it breaks
search, embeddings, and the viewer for every tainted bead, and does nothing for
copies already in `events` and `mutation_history`. Revisit once §6.5's cascade has
run in anger and the table list is stable.

**D. Approval via the SCIF portal pattern (named external writer).** Rejected as the
general mechanism. It hardcodes one fleet subject and an exact field allowlist; taint
owners are arbitrary operators. The escalation replay path is the right primitive,
with the three corrections in §6.3.

**E. Scrub as soft delete.** Rejected. Soft delete preserves every copy by design and
is restorable.

## 13. Open questions

1. **Primary static token.** Resolved. Applied phase 1 (`p1-read-gate-primary-token`,
   gd-kyWb3GYEwN) with a `BEADS_TAINT_PRIMARY_TOKEN_GRANTS=*` env escape for the
   migration window. The escape was removed in phase 3 (gd-3KUNlqC1SQ) once bridges
   held fleet-token grants of their own; the primary token is now gated like any
   other principal, with no bypass.
2. **gRPC.** The gRPC surface is pinned for freeze or removal. This ADR adds no gRPC
   RPCs and the read gate is applied in the shared server methods, so gRPC reads are
   covered; gRPC `AddLabel`/`RemoveLabel` remain unguarded and unrelated to taints.
3. **Do humans acquire taints?** No in this design: humans hold grants, agents carry
   taints. A human who reads production data and pastes it into an untainted bead is
   the one case propagation cannot see. Mitigation is the same as today (redaction
   on prose writes); a future `kd create --taint` habit and a viewer banner help.
4. **Coop recordings, logs, traces.** Out of scope. The recording redactor already
   has a hook point; a phase-4 task adds "recording carries the agent's taints and a
   purge deletes it". Logs and traces are the PII-gateway's problem.
5. **Cross-tenant taint names.** `taints.name` is global. If two tenants both want
   `production`, names need a fleet prefix. Decide when the second tenant exists.
6. **Trusted subjects and `inherit:read`.** Resolved 2026-09-20 in §15.3: keyed
   on the taint's effect set, never on a grant attribute; bounded by a narrow
   write surface and a system-executed lift; single-use reviewers are the
   fallback.
7. **`evict` and a running coop turn.** A `session_gates` row parks an agent at
   its next daemon call. Whether the controller can halt a coop session mid-turn
   is unverified; until it is, `evict` is "stop at next call", not "stop now".
8. **Per-effect grants.** A grant tolerates every effect of its taint. If a case
   appears where a principal should tolerate `gate` but not `restrict` on the
   same taint, split the taint rather than the grant; revisit if that becomes
   common.

## 14. Phased plan

| Phase | Lands | Proves |
|---|---|---|
| 1 | `type:team` bead + `kd team`; migration (`taints`, `bead_taints`, `taint_grants`, `taint_audit`); `kd taint create/grant/apply/show` with presets; propagation hooks (`create`, `assign`, `write`, `parent`); per-project `intake_taints`; read gate on by-id, list, search, events, graph; bead-page badges | story 1 end to end; an agent working a tainted bead cannot leak it to an ungranted reader |
| 2 | `inherit:read`; `scrub-request` type with born-inert guard; approve path with the three corrections; `ScrubTaint` cascade with the schema-diff test; `kd scrub …`; artifact; grant requests via escalation; viewer registry, taint detail, inbox card | story 3 with a human approver |
| 3 | policy evaluator, `auto` mode, caps; attestation check; the scrub formula; primary-token grant escape removed | story 2 and the automated story 3 |
| 4 | coop recording taint + purge; `/me/taints` and team pages; Slack notifications; bridges honouring `intake_taints` | jira-prod and acme-prod arrive tainted from the bridge, not by hand |

Each phase is its own MR series on the private mirror, reviewed there before
anything is public.

## 15. Effects, tolerations, and scope

Added 2026-09-20, after phase 1 landed. This section names two things the
original design left implicit — that grants are the toleration side of a taint,
and that a taint has exactly one effect — and reserves room for more effects
without building any. Nothing here changes phase 1 behaviour. The only schema
change is the `effects` column in §3, defaulted so every existing row means what
it meant before.

### 15.1 Scope: containment, not access control

Taints answer three questions: where did this information go, who may see it,
and what may be done on the strength of it. They do not answer who may perform
which operation in general — roles, `write_policy`, and project scope keep that
job. A design that tries to replace those with taints ends up hand-managing an
access list per taint, because taints only accumulate and grants are the only
cheap relief.

Taints may nevertheless be **universal**. A per-tenant intake taint (§10.3) sits
on every bead at steady state, and that is the intended end state, not a
degradation. The "empty registry" short-circuit (§10.1) is a performance
optimisation for installs that have not configured taints; it is not a
correctness assumption, and the all-tainted case must perform on its own — the
`bead_taints` primary key already serves the per-bead lookup, and the list
predicate joins on it.

The constraint that actually matters is this: **a taint must either partition
principals or be lifted at the rate it is applied.** A partitioning taint is one
no principal legitimately spans — one agent never serves two tenants. A taint
that is applied constantly and lifted rarely saturates: everything carries it,
audiences shrink to intersections, and the marker stops carrying information.
That is creep, and it is a property of the *pair* (apply rate, lift rate), not
of how many beads are tainted.

The closest working precedent is a government classification system, which
marks every document and still functions. The vocabulary maps directly, and
the mapping is a useful check on any new taint proposal:

| Classification concept | Here |
|---|---|
| Compartment (codeword) | a taint. This design is compartment-shaped: a set, not a ladder. A hierarchy of levels is modelled as compartments whose higher grants imply the lower ones. |
| Clearance + need-to-know | a grant to a team or operator; an agent grant bounded to its assignment (§5.2) |
| Original classification authority | the owner team |
| Derivative classification; the "derived from" line | propagation on read and write; `bead_taints.source` and `source_bead_id` |
| High-water mark: the compound document takes the higher marking | `inherit:write` taints the target rather than blocking the write |
| Dissemination controls (NOFORN, REL TO) | the `sink` effect; grants to named principals |
| ORCON, originator-controlled | grants issued only by the owner team — already the rule |
| Tearline: an unclassified portion above the line | the `mask` denial style on `gate` |
| Declassification schedule | a `scrub_policy` `auto` rule keyed on `older_than_days` |
| Downgrade or declassification review | `detaint`, in whichever mode the owner's policy selects (§6.1, §15.4) |
| Overclassification | creep. The remedy there, mandatory declassification review, is lift throughput here. |

### 15.2 Effects

`taints.effects` is a set. Only `gate` is enforced today; the others are
reserved with the semantics below so that a future taint can be proposed
against them. The empty set is legal and means the taint propagates and is
visible in provenance but changes no behaviour: a dye for tracing information
flow.

| Effect | Behaviour | Enforcement point | Status |
|---|---|---|---|
| `gate` | Callers without a grant cannot read or write the bead. By-id denial is 404 (§5.3). | `taint_read_gate.go`, `taint_write_gate.go`, the list predicate | implemented |
| `restrict` | The bead is readable by anyone the other gates allow. An **agent carrying** the taint may not perform the operations listed in the taint's `restricted_ops` — PR merge, privileged spawn, escalation approval, external send. An integrity marking: the data is untrusted, so what may be done on the strength of it is the question, not who may see it. | a check on the escalation and spawn paths keyed off `taintContextOf` (the agent's own taint set, already resolved per request) | reserved |
| `evict` | Applying the taint to a bead, or a grant expiring or being revoked, parks any agent that is assigned the bead or carries the taint without a grant. | a `session_gates` row written from the apply and revoke paths, released when a grant appears or a scrub completes; see §13 open question 7 | reserved |
| `sink` | Content never leaves the daemon by a named channel: embeddings, the Slack bridge, exports, coop recordings. Per channel, not per principal. | each writer checks bead taints before emitting | reserved |

`mask` is not an effect. It is a denial style on `gate` under which an ungranted
reader sees ids, edges, status and timestamps but blanked prose. It surrenders
invariant 9 (denials do not leak existence) for that taint, so it is per-taint
opt-in and defaults off.

### 15.3 Grants are tolerations, and what they tolerate depends on the effect

A grant is the toleration for a taint. One grant tolerates every effect of its
taint (§13 open question 8). What "tolerate" means differs by effect, and the
difference is the substantive content of this section:

| Effect | A granted principal… | …and on read |
|---|---|---|
| `gate` | may read and write the bead | **still acquires** the taint if it is an agent (`inherit:read`). It is cleared, and it is now contaminated; both are true. |
| `restrict` | may perform the restricted operations while carrying the taint | **does not acquire** the taint. It is a trusted subject: its outputs are its own, not derivatives of what it read. Without this a reviewer's verdict would itself be marked untrusted and the loop could never close. |
| `evict` | is not parked | as `gate` |
| `sink` | nothing changes; a grant does not open a channel | as `gate` |

The `restrict` row is the one place this design departs from "reads always
contaminate." It is the Biba trusted-subject rule and it is required for any
integrity taint to be liftable by review. Decided 2026-09-20 (closes §13 open
question 6): the exception is **keyed on the taint's effect set, not on an
attribute of the grant**. A grant on a taint whose effects exclude `gate`
suppresses acquisition; a grant on any taint that includes `gate` never does.
A grant that lets a principal act but still marks it is a grant nobody would
understand, so it is not expressible.

A trusted subject is also the principal a prompt injection most wants to reach:
it reads the untrusted material and is immune to the marker that would have
stopped it from acting. So it is bounded in ways an ordinary agent is not:

- **Narrow job on a higher tier.** The reviewer role of the agent review loop
  (epic gd-3UQKfj0nA8) is the intended holder: one task, stronger model.
- **Narrow write surface.** While acting as a trusted subject, an agent writes
  a verdict onto the escalation or decision bead and nothing else. The SCIF
  approval pattern (`scif_approval_authz.go`, an exact field allowlist for a
  named subject) is the existing primitive; a trusted subject does not create
  arbitrary beads.
- **The lift runs as the system.** The approve path replays under
  `WithMachineOperator`, which carries no taints. The reviewer vouches; the
  system removes the marker. The reviewer never touches `bead_taints`.
- **Humans already are trusted subjects.** §13 open question 3: humans hold
  grants and never acquire taints. This rule extends an exception humans
  already have to one narrow class of agent; it does not create a new kind of
  exception.

Fallback if the branch proves too hard to make safe: no trusted subjects at
all, reviewers acquire the taint like everyone else, the system lift clears the
bead including the verdict, and reviewers are single-use agents spawned per
review so the mark on the reviewer bead never matters. Costlier, but it keeps
"reads always contaminate" absolute.

Mechanically this is one branch inside the `inherit:read` hook (phase 2):
when deciding whether an agent acquires a taint on read, skip acquisition when
the taint's effects exclude `gate` and the agent holds a grant for it.

### 15.4 A third detaint mode: `lift`

`fork` and `strip` (§6.1) both exist to keep a confidentiality pre-image from
surviving. An integrity taint has no pre-image to protect: the content was
never secret, only untrusted. For a taint whose `effects` contain no `gate`,
`scrub_policy.detaint_mode: lift` removes the `bead_taints` row in place with
no content cascade, and writes the usual `taint_audit` row. `ValidateScrubPolicy`
refuses `lift` on any taint whose effects include `gate`. This does not touch
the fork-versus-strip question for `gate` taints (decision gd-Swd7mnGf3L).

For a high-rate integrity taint, `lift` must be triggered by the approval that
already exists for the underlying action — the reviewer approving the merge
escalation *is* the lift — never by a separate scrub request per bead. That is
the lift-rate condition of §15.1 applied.

### 15.5 Candidate taints checked against §15.1

| Taint | Effects | Applied by | Lifted by | Partition or lift-rate |
|---|---|---|---|---|
| tenant intake (`acme`, per §10.3) | `gate` | bridge and spawn under the project | rarely; purge on retention | partitions, provided one agent never serves two tenants — a scheduling rule, not a taint rule |
| `untrusted-input` | `restrict` | beads born from external Slack users, foreign GitHub issues, web fetches | `lift` on review | low apply rate; lifts as reviewed |
| `unreviewed` | `restrict` | every write by an agent that does not hold the grant | `lift` by the reviewer's merge or plan approval | apply rate is nearly all agent output, so it is sane **only if** the lift is the existing approval and costs nothing extra. Its advantage over a positive `reviewed` flag is that a later untrusted edit invalidates the review automatically and a plan derived from an unreviewed spec inherits the marker. Granularity is bead-level, so a low-tier comment re-marks a reviewed bead; scope `inherit:write` for this effect to content fields, not comments. |
| `credential-leak` | `gate`, `sink` | automatically, when `cred_redact.go` fires on a write | purge after rotation, `auto` rule | rare; turns a lossy redaction into a tracked incident naming the agent session that saw the secret |
| `trace-<name>` | none | by hand, on one seed bead | retire the taint | gates nothing; the Beads tab shows where information flowed |

### 15.6 What this section does and does not decide

Decided: the `effects` column and its default; that grants are tolerations
with per-effect meaning; the trusted-subject rule, keyed on effects and bounded
as in §15.3; that `lift` is a legal detaint mode only for non-`gate` taints;
the scoping rule in §15.1. Not built: any enforcement of `evict` or `sink`;
a per-taint `restricted_ops` schema; any candidate taint but `untrusted-input`.
`restrict` enforcement and `untrusted-input` are §15.7. Each of the rest is its
own bead against this section when it is wanted.

### 15.7 `restrict` enforcement and `untrusted-input` (as built, Lands 10.1)

Lands Track B step 10.1 (gd-g77WcV22SK) built the first `restrict` check,
narrower than the §15.2 row:

- **One shared check.** `kbeads/internal/server/restricted_ops.go`
  (`restrictedOpDenial`) rules on named ops. It reads the agent's role tiers
  (the lands-tenant table, Lands spec §5.1) and the agent's taint set.
- **What `restrict` refuses.** An agent carrying a taint whose effects include
  `restrict`, and holding no grant for it (§15.3: a grant tolerates), is refused
  `escalation_replay`: an approved escalation's operation does not run on its
  behalf. A non-human privileged spawn only ever runs as such a replay, so this
  covers it. An agent that is not a lands-tenant agent is also refused `spawn`
  (an agent bead create, or an update that sets its role), `escalation_approve`
  and `type_write` of a bead whose type configures the fleet or is acted on by
  a bridge or the controller (`restrictTaintFleetTypes`: config, role, user,
  advice, trigger, reaction-rule, dashboard, spawn_policy and the like), or of
  a `PUT /v1/configs` key (gd-gaGdksntAG). A tenant agent is held to its tier
  for those instead: its steward always carries the taint, so the taint would
  close the spawn and approve carve-outs the tier admits. Filing an escalation
  and rejecting one stay open, so an agent that read untrusted input can still
  ask, and the merge desk can still move a tenant's PRs. What the daemon does
  not see is out of reach: a merge, terragrunt or mint_secret escalation runs
  in the bridge, not at replay, and a merge made with an agent's own repo token
  is no daemon op at all. The op set is one list in code (`restrictTaintOps`),
  the same for every `restrict` taint; a per-taint `restricted_ops` field is
  not built.
- **The denial.** HTTP 403 with `denied_reason: taint_restrict`, the taint
  names in `taint_names`, never escalatable. On the replay path the refusal is
  recorded as the escalation's `result` (`result_status: failure`).
- **`untrusted-input`.** Every bead created in a tenant land (project bead with
  `land_kind=tenant`) carries it from creation, as if the land's `intake_taints`
  named it (source `inherit:project-intake`). `inherit:read` then carries it onto
  every agent that reads such a bead. The daemon does not mint the taint (the
  registry needs a live owner team); the operator creates it once with
  `kd taint create untrusted-input --owner <team> --effect restrict`; the
  registry refuses that name without `restrict` or with `gate` (which would hide
  a land's Commons from its own members). The daemon fails closed on it
  (gd-Ms3cmrdW49): while the taint is unregistered or not restrict-only, a bead
  create in a tenant land is refused, and so is a project write that makes a
  bead a tenant land (a create with `land_kind=tenant`, or an update that sets
  it; an existing land's other edits, a freeze among them, go through). Each
  refusal is a 403 with `denied_reason: untrusted_input_unusable`, never
  escalatable, logged as an error and counted in
  `gardenio_lands_untrusted_input_unusable_total{land,reason}`; the
  `LandUntrustedInputUnusable` alert (the Lands abuse rule) fires on the first.
  Refusing the land is what covers an empty registry, where the create path
  looks no project up (§10.1): the registry never shrinks, so no tenant land
  can exist before the taint does.
- **Where `inherit:read` fires** (gd-WdnPW8TLPq). On every read that hands an
  agent a bead's text, after the read gates have filtered, HTTP and gRPC alike:
  get by id, the list (`kd list`, `kd search`), semantic search, `/similar`
  neighbours, batch get, `/ready`, `/graph`, decisions and escalations (list and
  by id), the my-work summary (with or without an operator id), the taint
  detail tabs (`/v1/taints/{name}/beads` and `/scrub-requests`), the global
  `/v1/events` feed, each event written
  to the `/v1/events/stream` SSE stream, the referenced beads in `/mentions` and
  `/mentioned-by`, and every per-bead sub-resource that routes through
  `subresourceParentReadable` (comments, events, dependencies, labels,
  mutations, followers, operator history). A list page costs one batched
  `bead_taints` read (`propagateReadTaintBeads`); nothing is read for a
  non-agent caller or an empty registry. A sub-resource read of a bead the
  taint gate hides acquires nothing, since the acquired taint would show that
  the bead exists. A store error fails the read. Not covered: aggregates that
  return no bead text (stats, counts, `children-by-status`), the roster, the
  JetStream `/v1/bus/events` stream, and text that never passes through a bead,
  such as a tenant repo's PR bodies and diffs read with a GitHub token.
- **Which grants tolerate.** Only a grant to the agent bead itself (principal
  kind `agent`). An agent's read-gate grants include its spawn-chain root
  human's (§5.1), but a `restrict` taint ignores those, both in the
  restricted-ops check and in the §15.3 trusted-subject skip on read
  (`agentOwnGrants`, gd-Ms3cmrdW49). Otherwise a grant to the operator, or the
  operator joining the taint's owner team, would make every agent the operator
  started a trusted subject that never acquires the taint and passes the check.
  The grant set is resolved for the agent the op runs as, which at replay is
  the requester, not the approver.
- **Lifting.** Not automated. A reviewer removes the taint from the agent bead,
  or the taint's owner grants it to a trusted agent bead (for example the merge
  desk) so it never acquires it (§15.3's trusted-subject rule). Lifting on a
  reviewer's merge approval (`detaint_mode: lift`) is follow-up work.
