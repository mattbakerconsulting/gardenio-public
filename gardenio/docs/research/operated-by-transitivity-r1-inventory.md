# R1: Inventory of `operated_by` Propagation Paths

- **Bead:** ka-0000005258
- **Parent epic:** ka-0000003404 (operated_by transitivity, "dibs" rule, audit trail)
- **Related ADR:** `docs/adr/human-attribution-propagation.md` (R3, the canonical design-of-record)
- **Status:** Research only. No code changes.

## Goal

Trace every spawn / create / mutate path that touches `operated_by` today,
identify which paths correctly propagate the human operator and which drop it,
and surface gaps that R2/R3/R4 will need to close.

The R3 ADR established the trust model and the resolver ladder
(OIDC → `X-Operated-By` header → agent-bead `slack_user_id` fallback →
empty). This R1 doc is the call-site census — it maps each call site onto
that resolver and records, per row, whether the operator survives the call.

## Scope

In-scope: anything that ends up writing a row whose `operated_by` column we
care about — beads (issue/data/agent/decision), comments, deps, events, and
mutation_history. Out-of-scope: read-only views, embeddings, references
extraction.

## Resolver recap (R3 §4)

The server is the final arbiter. Every write goes through
`(*BeadsServer).resolveOperatedBy` in `kbeads/internal/server/audit.go`:

1. **OIDC** caller → `oidc:<email>` (`operated_bySource = oidc_subject`).
2. **Static-token** caller with sanitized `X-Operated-By` → header value
   verbatim (`static_header`).
3. **Static-token** caller without header → look up the calling agent's bead
   by Subject, read `Fields["slack_user_id"]`, return `"slack:"+id`
   (`agent_bead_fallback`).
4. Otherwise → `""` (`empty`).

Client plumbing: `gb` and `kd` set `BEADS_OPERATED_BY` env (read by
`defaultOperatedBy()`) → CLI flag `--operated-by` overrides → client default
applied via `SetOperatedBy(op)` → per-call override via `WithOperator(ctx, op)`
which the HTTP/gRPC client lifts into `X-Operated-By`. Pod env injection
(`podmanager/manager.go:758`) writes `BEADS_OPERATED_BY` from
`spec.OperatedBy` so children inherit the operator transparently.

A propagation gap, then, is a call site where:

- the caller is a static-token client (server, bridge, gardener), AND
- it does not stamp `WithOperator` and the bead it writes does not yet have a
  `slack_user_id` field for the resolver to fall back to.

The resolver then returns `""` and that row is silently anonymous.

## Inventory

Legend:
- ✓ — operator survives end-to-end (no gap).
- △ — operator survives for the synchronous write but the **bead persisted to
  the DB** does not carry the field needed for downstream propagation
  (children, reconciler, agent-pod env). The row's `operated_by` column is
  populated; later writes by descendants will fall through.
- ✗ — gap. The DB row's `operated_by` resolves to `""`.

| # | Caller (file:line) | Bead/object touched | `operated_by` source today | Gap | Proposed source |
|---|---|---|---|---|---|
| 1 | `scheduler.spawnScheduledAgent` (controller/internal/scheduler/scheduler.go:1087) | new task bead under schedule | resolver: static-token, no header, agent-bead lookup against scheduler subject → empty | ✗ | inherit from schedule bead's `operated_by` (or `slack_user_id` field if present); stamp `WithOperator(ctx, schedule.OperatedBy)` and copy `slack_user_id` into `taskFields` so downstream agent inherits |
| 2 | same — agent-bead spawn via `daemon.SpawnAgent` (scheduler.go:1130) | agent bead | same as #1 | ✗ | same — also include `slack_user_id` in `schedFields` so future child writes can use the agent-bead fallback |
| 3 | Slack `/spawn` (`bridge/bot_commands.go:520`) → `daemon.SpawnAgent` | agent bead (initial create) | `WithOperator("slack:"+cmd.UserID)` set on ctx (line 47) → header → `slack:U…` | △ | the agent bead's **Fields** do not yet include `slack_user_id` at create time (added in a follow-up `UpdateBeadFields` at line 533); fold those fields into the create-time `extraFields` so the bead is consistent in one round-trip and the agent-bead fallback works for the agent's first writes |
| 4 | Slack mention handler (`bridge/bot_mentions.go:34`) | comments / created beads in mention flow | `WithOperator(ctx, "slack:"+ev.User)` ✓ | ✓ | (none) |
| 5 | Slack reaction handler (`bridge/bot_reactions.go:90`) | reactions, comments | `WithOperator(ctx, "slack:"+ev.User)` ✓ | ✓ | (none) |
| 6 | Slack interactive callbacks (`bridge/bot.go:777,1057,1117`) | decision responses, comments | `WithOperator(ctx, "slack:"+callback.User.ID)` ✓ | ✓ | (none) |
| 7 | JIRA mention → CreateBead (`bridge/jira_mentions.go:193`) | mention task bead | `WithOperator(ctx, "jira:"+accountID)` ✓ | ✓ | (none — but see #8) |
| 8 | JIRA mention → SpawnAgent (`bridge/jira_mentions.go:255`) | spawned agent bead | header set; **Fields lack `jira_user_id` / `slack_user_id`** | △ | add a `jira_user_id` field to `extraFields` and have the resolver's fallback understand JIRA identities (or re-stamp via env in pod build) |
| 9 | JIRA sync / webhook / squawk-forward (`bridge/jira_sync.go`, `jira_webhook.go`, `jira_squawk_forward.go`) | comments, beads created from JIRA events | no `WithOperator` anywhere → static-token → resolver fallback against bridge subject → empty | ✗ | derive `jira:<accountId>` from the source JIRA event author and `WithOperator` it; for system-driven syncs (no human author), use `system:jira-sync` |
| 10 | GitLab mention → SpawnAgent (`bridge/gitlab_mentions.go:282`) | spawned agent bead, mention task | **no `WithOperator` in this file** → empty | ✗ | derive `gitlab:<userId>` from MR author / commenter, `WithOperator` it, and add `gitlab_user_id` to `extraFields` |
| 11 | GitLab sync / squawk-forward / generic (`bridge/gitlab_sync.go`, `gitlab_squawk_forward.go`, `gitlab.go`) | comments, beads | same as #10 | ✗ | same — propagate `gitlab:<userId>` or `system:gitlab-sync` |
| 12 | `gb spawn` CLI (`controller/cmd/gb/spawn.go:191`) | agent bead | client default `BEADS_OPERATED_BY` env → header → resolver returns operator. **But `extraFields` carries `spawned_by`, not `operated_by` / `slack_user_id`** — so the agent's Fields do not preserve the operator for its own future writes (pod-env injection sees `spec.OperatedBy == ""`) | △ | populate `extraFields` with `operated_by` and (if available) `slack_user_id` so `operatedByFromMetadata` / `BuildSpecFromBeadInfo` can carry the operator into the spawned pod's `BEADS_OPERATED_BY` env |
| 13 | `kd` CLI arbitrary writes (`kbeads/cmd/kd/*`) | any bead/comment/dep | `--operated-by` flag or `BEADS_OPERATED_BY` env → header ✓ | ✓ | (none — R3 path works) |
| 14 | Reconciler `BuildSpecFromBeadInfo` (`controller/cmd/controller/podspec.go:38`) | pod spec env (read-only) | reads `meta["operated_by"]` then falls back to `"slack:"+meta["slack_user_id"]` | ✓ | (none — but its correctness depends on upstream writers populating one of those fields; gaps #1, #3, #8, #10, #12 starve it) |
| 15 | Trigger engine `actionSpawnAgent` (`bridge/triggers.go:547,571`) | agent bead from JIRA/GitLab/webhook/etc. trigger | `extraFields` includes `spawned_by: "trigger:"+trig.ID`. No `WithOperator`. No `operated_by` field. | ✗ | thread the source event's actor (`jira:<accountId>` / `gitlab:<userId>` / `slack:<userId>`) into `WithOperator` and `extraFields["operated_by"]`; for schedule-driven triggers, inherit from the trigger's own `operated_by` (per #1) |
| 16 | Trigger engine `actionCreateBead` / `actionCreateBeadWithFormula` (`bridge/triggers.go:598,608`) | new task / formula-pour molecule | no operator threaded; `trigger_created_by` is recorded as a separate field but not as `operated_by` | ✗ | same — propagate event actor → `WithOperator` + `extraFields["operated_by"]`. R3 §6.4 says formula fan-out should default to inheriting the caller's header, which works once the caller stamps it. |
| 17 | Trigger engine `actionNotify` (`bridge/triggers.go`) | mail / notification beads | no operator | ✗ | same |
| 18 | Formula apply (`kbeads/cmd/kd/formula_apply.go`) | each pour creates N beads | each `CreateBead` inherits the caller's `X-Operated-By` header — no parent→child mechanism | ✓ | (none — R3 §6.4 documents this as the right default. The expansion is a fan-out from the caller, so the caller's operator is the right one for all children.) |
| 19 | Molecule `pour` / `wisp` (`kbeads/cmd/kd/mol_*.go`) | parent molecule + step beads | same as #18 | ✓ | (none) |
| 20 | Decision response (`gb decision report`, `bridge/bot.go` decision callbacks) | decision bead update + comment | OIDC for `gb`; `slack:U…` via `WithOperator` for Slack callback | ✓ | (none) |
| 21 | Agent → child writes (in-pod CLI calls) | comments, child beads, deps | pod env `BEADS_OPERATED_BY` set by `manager.go:761` from `spec.OperatedBy` → CLI default → header ✓ | ✓ | (none — provided the spawning row populated `operated_by` or `slack_user_id`; see gaps #1, #3, #8, #10, #12) |
| 22 | Gardener self-spawn / re-entry (scheduled-gardener re-fire) | gardener agent bead, child task beads | identical to #1 — gardener is born without an operator | ✗ | same fix as #1 — gardeners need to inherit their schedule bead's operator |

## Summary of gaps

Hard gaps (✗) — DB row's `operated_by` resolves to `""` today:

- **Scheduler-driven spawns** (#1, #2, #22) — single largest source. Every
  cron / scheduled gardener run produces operator-less task and agent beads.
- **GitLab bridge** (#10, #11) — entire surface area is missing
  `WithOperator`. Slack and JIRA-mention paths are partially correct; GitLab
  is uniformly empty.
- **JIRA non-mention paths** (#9) — sync, webhook, and squawk-forward never
  thread an operator.
- **Trigger engine actions** (#15, #16, #17) — `spawn_agent`, `create_bead`,
  `apply_formula`, `notify` all drop the source event's actor.

Soft gaps (△) — synchronous write is correctly attributed, but the resulting
bead's Fields are insufficient for downstream propagation (pod env, child
writes via fallback):

- **Slack `/spawn`** (#3) — fold `slack_user_id` into the initial
  `extraFields` instead of a follow-up `UpdateBeadFields`.
- **JIRA mention spawn** (#8) — add a `jira_user_id` (or generalized
  `operator_id`) field; teach the resolver fallback to handle non-Slack
  identities.
- **`gb spawn`** (#12) — the CLI knows the operator but only stamps
  `spawned_by`. The spawned pod's `BEADS_OPERATED_BY` env will be empty
  unless we also populate `operated_by` / `slack_user_id` in `extraFields`.

Working paths (✓) that R2/R3 should preserve as-is:

- All Slack event handlers except `/spawn`'s two-step pattern.
- JIRA mention CreateBead.
- `kd`/`gb` CLI arbitrary writes (the R3 env+header path).
- Formula apply / molecule pour (R3 §6.4 default).
- Decision responses.
- Agent → child writes via pod env (R3 path).
- Reconciler read-only pod-env derivation (`operatedByFromMetadata`).

## Cross-cutting observations

1. **Two field names for one concept.** `spawned_by` (in `gb spawn`,
   `actionSpawnAgent`) is a near-synonym for `operated_by` but is **not**
   what the resolver, pod-env injection, or audit column read. R2 should
   either alias them or migrate writers to `operated_by`.
2. **The two-step Slack `/spawn` pattern (#3) is fragile** — the agent bead
   exists for one round trip without a `slack_user_id`, and any side-effect
   that triggers off the bead's creation event (reconciler, gardener
   matchers) will see an operator-less row.
3. **No write path generates `gitlab:` or generalizes the `jira:` form**
   beyond the one mention handler. The resolver fallback only knows
   `slack:`. R3 should specify the canonical operator-id grammar
   (`<provider>:<id>`) and update the fallback to read whichever
   `<provider>_user_id` field is present, not just `slack_user_id`.
4. **System-driven syncs (JIRA sync, GitLab sync) need a sentinel.** When
   there is no human author (e.g. periodic poll), the right value is
   probably `system:jira-sync` / `system:gitlab-sync` rather than empty —
   so the audit log distinguishes "we didn't bother" from "no human was
   involved".
5. **Formula expansion is correct by R3 §6.4 default** — explicit
   parent→child propagation would be a regression. Leave it.
6. **The `actionCreateBead` `trigger_created_by` field is parallel
   plumbing** to `operated_by` and should be reconciled.

## Follow-ups for R2 / R3 / R4

- **R2 (transitivity rules):** define propagation matrix per source class —
  human-actored events (Slack/JIRA/GitLab user actions) inherit the actor;
  schedule-driven events inherit the schedule's own `operated_by`;
  reconciler-driven recreations inherit the spawned bead's persisted field;
  truly autonomous system writes use `system:<name>`.
- **R3 (dibs rule for the audit column):** the existing R3 ADR documents
  sticky-on-insert. R1 confirms most gaps are at insert-time, not at
  update-time, which keeps R3's "dibs" simpler — fix the inserts.
- **R4 (audit trail):** every gap row above will currently emit
  `operated_by_source = empty` in the per-write metric (M2 in
  `audit.go:181`). R4 dashboards should rank top-`empty`-source RPCs to
  surface regressions.
- **Backfill (R3 §10):** the gaps inventory above is the menu of historical
  rows that will need `kd admin reextract-references`-style backfill once
  the inserts are fixed. The reconciler's `operated_by` derivation
  (#14) reads what is on the bead, so backfilling Fields will retroactively
  give pods the right env.

## Source citations

- ADR: `docs/adr/human-attribution-propagation.md` §4 (trust), §6
  (transitivity edge cases), §10 (call sites).
- Resolver: `kbeads/internal/server/audit.go`
  (`resolveOperatedByPure:187`, `operatedByFromAgentBead:217`).
- Auth interceptor: `kbeads/internal/server/interceptors.go:220–320`.
- Pod-env injection: `gardenio/controller/internal/podmanager/manager.go:758`.
- Pod-spec derivation: `gardenio/controller/cmd/controller/podspec.go:38–54`.
- Client plumbing: `gardenio/controller/internal/beadsapi/client.go:184–203`.
- Scheduler: `gardenio/controller/internal/scheduler/scheduler.go:1087`.
- Slack `/spawn`: `gardenio/controller/internal/bridge/bot_commands.go:47,520,533`.
- Slack mentions: `gardenio/controller/internal/bridge/bot_mentions.go:34`.
- Slack reactions: `gardenio/controller/internal/bridge/bot_reactions.go:90`.
- Slack callbacks: `gardenio/controller/internal/bridge/bot.go:777,1057,1117`.
- JIRA mention: `gardenio/controller/internal/bridge/jira_mentions.go:193,255`.
- JIRA other: `gardenio/controller/internal/bridge/jira_sync.go`,
  `jira_webhook.go`, `jira_squawk_forward.go` (no `WithOperator`).
- GitLab: `gardenio/controller/internal/bridge/gitlab_mentions.go:282`,
  `gitlab_sync.go`, `gitlab_squawk_forward.go`, `gitlab.go` (no `WithOperator`).
- `gb spawn`: `gardenio/controller/cmd/gb/spawn.go:191–208`.
- CLI flag plumbing: `gardenio/controller/cmd/gb/main.go:26,45,115`,
  `kbeads/cmd/kd/main.go:20,47,111`.
- Trigger engine: `gardenio/controller/internal/bridge/triggers.go:547,598,608,627`.
- Formula apply: `kbeads/cmd/kd/formula_apply.go`.
