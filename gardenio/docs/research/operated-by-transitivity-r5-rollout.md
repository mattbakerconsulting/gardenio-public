# R5 — Cross-cutting rollout sketch for `operated_by` transitivity

- **Bead:** ka-0000006418
- **Parent epic:** ka-0000003404 (operated_by transitivity, dibs rule, audit trail)
- **Sibling research:** R1 inventory (`operated-by-transitivity-r1-inventory.md`),
  R2 propagation (`operated-by-transitivity-r2-propagation.md`),
  R3 dibs rule (`operated-by-transitivity-r3-dibs.md`),
  R4 audit (`operated-by-transitivity-r4-audit.md`).
- **Status:** Research only. No code, no schema, no tests. The full
  implementation rollout belongs to the follow-on impl epic gated by the
  parent's decision checkpoint.

## 1. Goal and scope

Assume R2–R4 have been implemented (push-at-spawn + pull-at-server hybrid;
dibs rule; audit substrate via mutation_history + view + `operated_by_set_at`
column). Sketch the order in which sub-systems pick up `operated_by`,
which flag knob gates each, and what falls back when the flag is off.

Out of scope: the migration / DDL choreography for the audit substrate (R4
§7–§8 owns it), bridge call-site patches (R1 owns the gap list, impl epic
owns the patches), test design (parent epic excludes test work), and the
viewer-default-filter epic (separate research, see V-R1..V-R4).

The flag knob is `BEADS_OPERATED_BY_ENABLED`, originally introduced in R9
(`docs/research/human-attribution-rollout.md`) and currently read in
`kbeads/internal/server/audit.go::operatedByEnabled`. R5 reuses that flag
verbatim — no new env var.

## 2. Cutover sequence

The chain has six load-bearing surfaces, ordered by *risk-adjusted cost of
landing first*: server-side reads/writes are the cheapest to flip (purely
additive when the flag is off), pod env and viewer chip are user-visible,
and the rebind side-effect is the most subtle. The dependency arrows form
a DAG, not a strict line — steps S1/S2 must precede everything; S3..S6
fan out.

```text
   S1 server resolver  ──┬─►  S2 push at spawn / create  ──┬─► S3 pod env stamping
                         │                                  ├─► S4 viewer chip + ownership tab
                         │                                  └─► S5 dibs rebind side-effect
                         └─►  (audit substrate from R4 already lit by S1's appends)
                                                            └─► S6 backfill / narrowing pass
```

| # | Surface | What lands | Flag gate | Fallback when flag is off |
|---|---|---|---|---|
| **S1** | **Server resolver + audit substrate** (`audit.go::resolveOperatedBy`, `mutation_history` appends, R4 view + `operated_by_set_at` column, R3 dibs rule) | The whole write-side machinery: pull, dibs, mutation_history append, view DDL. Migration adds `operated_by_set_at TIMESTAMPTZ DEFAULT NOW()` and the partial indexes from R4 §7. | `BEADS_OPERATED_BY_ENABLED` at the **server** (existing R9 short-circuit). | `operatedByFromContext` returns `""`; dibs rule no-ops on every branch (incoming is empty → branch 1); audit appends carry empty operator (already the column DEFAULT). Schema is in place but inert. |
| **S2** | **Push at spawn / create** (R1 ✗ rows: scheduler, `gb spawn`, JIRA mention spawn, GitLab handlers, trigger actions, `/spawn` two-step fold) | Each writer stamps `Fields["operated_by"]` and `Fields["<provider>_user_id"]` on insert; `WithOperator(ctx, …)` on the RPC ctx. | None — push writes are unconditionally safe (they only populate fields; readers tolerate empty). | Push lands in code; fields are populated even when S1 flag is off (the resolver still runs the new stamping; only the *resolution* short-circuits to `""`). When S1 flips on later, those fields are immediately useful. |
| **S3** | **Pod env stamping** (`podmanager/manager.go:758` reads `spec.OperatedBy`, derived from R1 row #14 path) | The reconciler's `BuildSpecFromBeadInfo` already consumes `Fields["operated_by"]` / `slack_user_id`. With S2 in place, pods boot with a non-empty `BEADS_OPERATED_BY` env. The CLI library (`kd`/`gb`) reads it on init. | Implicit on S2 (no separate flag). | If S2 hasn't reached a given spawn site, pod env is `""` and the CLI default is empty — same behaviour as today. |
| **S4** | **Viewer chip + ownership tab** (`beads-viewer`) | Chip reads `beads.operated_by` (already wired); ownership tab queries the R4 view (`bead_operator_history`) for transitions. `--ever-operated-by=me` toggle (R3 §5.5) defers to the next viewer epic. | Server flag (S1) is sufficient — when off, `beads.operated_by` is uniformly empty; chip renders the existing "no operator" placeholder; ownership tab shows an empty timeline. | Chip / tab degrade to empty state. No client-side flag needed. |
| **S5** | **Dibs rebind side-effect on decision-answer** (R3 §5.6, R2 §5.2 step 3) — the in-transaction update of the answering agent bead's `operated_by` to the responder, plus the pod-side refresh on `gb yield` return / `gb prime`. | Server-side rebind is gated by the same `BEADS_OPERATED_BY_ENABLED` flag (it lives in the dibs path). Pod-side `gb prime` refresh is unconditional — it just re-reads its bead, which is a no-op when the field is empty. | Server skips the rebind update; pod refresh re-reads an empty field; `BEADS_OPERATED_BY` env stays at its boot value (whatever push set or empty). Sentinel chains never transfer to humans, which matches today's behaviour. |
| **S6** | **Backfill / narrowing pass** (R7-style, scoped to *post*-S1 history) | One-shot job: replay `mutation_history` to seed `bead_operator_history` view consumers (none required if we stay on R4 Option C). Optional second pass: walk live `beads` rows and copy `operated_by` from the most recent non-sentinel mutation_history row to fill backfill gaps for beads created during S2's rolling deploy. | Independent operator-run job, gated on S1 + S2 having soaked. Off when not running. | If skipped, beads created during the S2 rollout window stay empty — they are already empty today, so this is a polish step, not a correctness step. R4 §2.4 explicitly excludes pre-cutover history. |

### 2.1 Why this order

S1 first because it is **invisible when the flag is off**. The resolver,
the dibs rule, the audit appends, and the schema migration are all
additive: the column DEFAULT is `''`, the partial indexes match no rows
when `operated_by=''` is excluded, the dibs rule branches all bail at
branch 1 when `incoming==""`. Landing S1 dark gives us the schema and
the code on every kbeads pod with zero behavioural change, so subsequent
flips are pure config.

S2 second because it is **safe with S1 dark**. Push writes are pure
field stamps; they don't change any read behaviour until S1's flag is
on. Landing S2 before flipping S1 means the very first flip already
sees populated Fields on freshly-spawned beads, and the chip lights up
immediately rather than after the next mutation triggers dibs.

S3 falls out of S2 — same code change in the reconciler; deploys
together with S2 in practice.

S4 needs the audit view (R4) and the partial indexes (S1 migration). It
should land in a `beads-viewer` deploy *after* the schema migration but
can ship dark (the chip just shows empty until S1's flag flips per env).

S5 last among the runtime surfaces because it is the most subtle: a
bug in the rebind side-effect can silently re-attribute an agent to the
wrong human. Landing it *after* S1 has soaked in dev/rwx with non-rebind
traffic gives us a baseline for the metric "% of agent beads whose
operated_by changes per day" before rebind starts pushing that number up.

S6 is optional polish; it can land any time after S1 + S2 have soaked
in production, or be skipped entirely.

### 2.2 Per-env timing

Same lineage as R9 §2.5:

| Env | Default | Flip after |
|---|---|---|
| **dev** (`gardenio-dev`) | flag **on** | S1 + S2 land in main; flip on first deploy. Soak ≥ 24h before promoting to rwx. |
| **rwx** (`gardenio-rwx`) | flag **on** | dev soak clean (no spike in `operated_by_source=empty` per the R4 / R9 dashboards). Soak ≥ 7d before prod. |
| **prod** (`gardenio`) | flag **on** | rwx soak clean; release gardener flips via `helm upgrade` with `featureFlags.operatedBy=true`. |

Defaults stay **off everywhere** until each env has explicitly flipped.
The flag is a single env var — no per-feature sub-flags. S5 (rebind)
inherits the same flag; we do not introduce a second knob to gate
rebind separately. Rationale: rebind is logically part of dibs, and a
half-on (dibs without rebind) state would surface as "agent kept acting
as the wrong human after a decision answer", which is more confusing
than the all-off state.

### 2.3 What falls back when the flag is off (per surface)

Restated tightly because this is the load-bearing safety property:

- **Server resolver:** returns `""`. All inserts land with `operated_by=''`
  (column DEFAULT), all dibs branches no-op. R3-era behaviour.
- **Push writes:** still happen (S2 is unconditional code), populating
  `Fields["operated_by"]` etc. Resolver ignores the header path → bead
  row column stays empty even though Fields are populated. When the flag
  flips, future writes pick up the populated Fields via the resolver's
  agent-bead fallback (R2 §3.4) without any backfill.
- **Pod env:** `BEADS_OPERATED_BY` derives from `spec.OperatedBy` which
  derives from `Fields["operated_by"]`. With S2 on and S1 off, the env
  IS set on the pod, but the CLI's `X-Operated-By` header is then
  rejected at the server resolver (returns `""`). Functionally identical
  to today, and there is no new agent-visible behaviour.
- **Viewer chip:** reads `beads.operated_by`; renders empty-state.
- **Dibs rebind:** server-side update skipped; pod refresh is a no-op.
- **Audit substrate:** `mutation_history` rows are written with
  `operated_by=''`. The R4 view returns rows with `operator_id=''`,
  which the viewer's ownership tab filters out (per R4 §6 / partial
  index `WHERE operator_id <> ''`).

The on-state is a *strict superset* of the off-state semantics. There is
no path where flipping the flag on can break a working off-state read
or write — that property is what makes the per-env soak strategy safe.

## 3. Flag strategy (where the gate lives)

**One flag, server-side: `BEADS_OPERATED_BY_ENABLED`.**

- **Server** (kbeads): primary gate. Read in `audit.go::operatedByEnabled`,
  short-circuits `resolveOperatedBy` to `""`. The dibs rule and the
  audit append both bail when `incoming==""` (R3 branch 1), so a single
  short-circuit covers the entire write path.
- **Client** (`kd` / `gb` CLI, agent pod env): **no client-side gate.**
  The client always stamps `X-Operated-By` from its env if present.
  When the server flag is off, the header is read and discarded. This
  is intentional: clients on out-of-sync deploys (older agent images
  pinned by a long-running schedule) keep working without coordinated
  re-rolls. When the server flag flips, those clients immediately start
  resolving correctly.
- **Bridges** (slack-bridge, jira-bridge, gitlab-bridge): same as
  client — always stamp. No bridge-side flag.
- **Reconciler** (`podmanager`, `controller`): no flag. Push code
  always populates `spec.OperatedBy` / Fields when the source bead
  carries them. Off-state pods boot with empty env if push hasn't
  reached the spawning site, which is identical to today.
- **Viewer** (`beads-viewer`): no flag. Reads `beads.operated_by`
  directly. When the server flag is off, that column is uniformly
  empty and the chip renders empty-state. No additional UI toggle.

Rationale for "server-only gate":

1. **Single point of control.** Per-env soak is a single
   `helm upgrade` flip. No multi-component coordination, no
   half-flipped failure modes.
2. **No coordination across deploy boundaries.** Agent images, bridge
   pods, viewer deploys, and CLIs in operator dotfiles can all be on
   different versions during a soak; they all keep working because they
   stamp the header unconditionally and the server is the arbiter.
3. **Identical to R9.** The `BEADS_OPERATED_BY_ENABLED` lineage from
   R9 was designed exactly this way; reusing it avoids introducing a
   second flag with a different gate site. Anything that needs more
   surgical gating (e.g. "rebind in dev only") should land as a
   *follow-on* knob inside the impl epic, not at R5 design time.
4. **Default-off is safe.** Pre-flag deploy behaviour is exactly the
   off-state behaviour by construction — the resolver returns `""`,
   the column DEFAULT is `''`, the partial indexes are empty.

The Helm wiring already exists in
`gardenio/helm/values-dev.yaml` and the kbeads deployment template; R5
extends it to `values-rwx.yaml` and `values-prod.yaml` as part of the
per-env flip. No template change needed beyond the per-env value.

## 4. Risk callouts

### 4.1 Backfill

Per R4 §2.4 and R7 lineage, **historic beads created before S1 + S2
land are not retroactively re-attributed.** They keep their empty
`operated_by`. The R4 partial indexes exclude them; the viewer chip
renders empty-state for them; the ownership tab shows nothing. This is
acceptable — the parent epic's hard rule is "no backfill in this
epic; that is a separate question."

Risk mitigation: the operator-facing dashboard from R9 should grow a
panel that distinguishes:

- `operated_by_source = empty, bead_created_at < S1_flip_at` — historic,
  expected to stay empty.
- `operated_by_source = empty, bead_created_at >= S1_flip_at` — a
  rolling-window failure to attribute. **This is the actionable signal.**
  If non-zero, a push site was missed (R1 inventory) or the resolver
  fallback failed.

The S6 narrowing pass is the only retroactive fill in scope, and only
for beads created in the S2 rollout window between code-deploy and
flag-flip. Even that is optional polish.

### 4.2 Sentinel agent (`system:scheduler`) chain-stop interactions

The biggest cross-cutting risk: when S1 flips on but S5 (rebind) lags,
**sentinel-rooted chains stay sentinel forever, even after a human
answers a decision.** Concretely:

1. Cron schedule (sentinel) spawns gardener. Gardener inherits
   `system:scheduler`.
2. Gardener creates decision `D`. `D.operated_by = system:scheduler`.
3. Bob clicks "Approve". Without S5: dibs flips `D.operated_by →
   slack:U_bob` (R3 branch 6) but the gardener's `operated_by` is NOT
   updated. The gardener's pod still has `BEADS_OPERATED_BY=
   system:scheduler` and continues stamping the sentinel.
4. The gardener's subsequent task creations land with
   `operated_by=system:scheduler`, which the dibs rule treats as
   transparent. The viewer chip shows nothing for those tasks even
   though Bob is now driving.

Mitigations:

- **Land S5 in the same release as S1 flip.** Do not flip S1 in prod
  if S5 has not soaked in rwx. The flag gates both because they share
  `BEADS_OPERATED_BY_ENABLED`, but the *code* for S5 must be present.
- **Telemetry:** emit a metric on every gardener pod that tracks
  "ratio of writes stamped with `system:*` vs human-class operator
  over a 1h window". A gardener that should have rebound but didn't
  shows as 100% sentinel even after a decision answer. Surface in the
  R9 dashboard.
- **Document the non-rule** (R3 §5.6.3): commenters do NOT rebind. A
  human comment on a sentinel-rooted gardener bead does not transfer
  the chain. This will be a discoverability gap; the viewer's
  ownership tab should include a "this agent is operating as
  `system:scheduler` — answer its open decision to take over"
  affordance. (Defer to viewer epic; flag here for awareness.)

### 4.3 Dibs rebind paths (race window during rollout)

The rebind side-effect (S5) is a transactional update of the agent's
`operated_by` plus a pod-side env refresh on the next `gb yield` /
`gb prime`. Two race-shaped risks:

1. **Decision answered while S1 flag is mid-flip across kbeads
   replicas.** If kbeads runs >1 replica and the flag is set via env
   var, a `helm upgrade` rolls them one at a time. A request landing
   on an old replica during the rollout sees the flag off; a follow-up
   on a new replica sees it on. The rebind transaction is atomic per
   request, so the worst case is "the decision-answer write resolves
   correctly but the gardener bead update is skipped because the flag
   was off on the replica that handled the side-effect call." Mitigation:
   roll kbeads replicas during a low-decision-traffic window
   (off-hours), and accept that a small number of decisions may need
   manual rebind via `kd field set <gardener-bead> operated_by slack:U_bob`.
   Document this in the per-env flip runbook.

2. **Gardener pod misses the env refresh.** The pod-side refresh
   mechanism (R2 §5.2 step 4) is owned by the impl epic; the simplest
   shape is "every `gb prime` re-reads the bead's `operated_by` and
   updates the env the CLI library will stamp". If a gardener runs
   continuously without ever calling `gb prime` (it should — the
   prime hook is wired to compaction / clear / new-session), it could
   miss the refresh and continue stamping the old operator until the
   next prime. Mitigation: the gardener entrypoint already invokes
   `gb prime` on session start, and the `gb yield` return path is the
   privileged rebind moment per R2 §5.2 step 4. The impl epic must
   ensure both call sites refresh; this is a code review item, not a
   rollout-time risk.

### 4.4 R1-row inventory drift

The R1 inventory was a snapshot. Between R1 landing and S2's impl
patches landing, new spawn / create call sites may have been added
(new bridge handlers, new trigger actions, new `gb` subcommands). The
S2 impl epic should re-run the R1 census against `git log --since
<r1-merge-date>` for any new caller of `daemon.SpawnAgent`,
`CreateBead`, or `actionSpawnAgent` and add it to the patch list.

Mitigation: a lint rule (or a code review checklist item) that any new
caller of those APIs must either (a) stamp `WithOperator` and populate
operator Fields, or (b) explicitly stamp a sentinel. "Forgot to
propagate" should be a build-time finding, not a runtime gap. (Lint
rule belongs to the impl epic, not R5.)

## 5. Open questions for the parent decision checkpoint

These should be folded into `gb decision create` against ka-0000003404
rather than answered unilaterally here:

1. **Single flag for S1 + S5, or separate?** R5 recommends single
   (§3 rationale). If the operator wants the option to flip dibs on
   without rebind, we need a second flag. Recommend: single. Add a
   second flag only if a per-env soak finds a real reason.
2. **S6 narrowing pass: in scope or skip?** It is optional polish per
   R4 §2.4. Recommend: skip for the initial cutover; revisit if the
   "empty since flip" panel shows a meaningful number of beads created
   during the S2 rollout window.
3. **Gardener pod refresh cadence.** Mirrors R2 §9 question 1. Same
   recommendation: refresh on `gb yield` return AND on `gb prime`
   invocation. Both already exist; no new lifecycle hooks.
4. **Per-env flip runbook ownership.** Is the release gardener
   responsible for flipping the flag per env, or does it ride along
   with the next chart bump? Recommend: ride with the chart bump
   (`featureFlags.operatedBy=true` in `values-<env>.yaml`), so the
   flip is reviewed as part of a normal release MR. No separate
   manual procedure.
5. **Halt criteria.** What signal causes us to flip the flag back
   off mid-soak? Recommend: any of (a) `mutation_history`-write
   error rate >1% (the dibs append is in the write path), (b)
   "% of writes stamped with `operated_by_source=empty`" rises >5
   percentage points above baseline, (c) gardener rebind-miss
   metric >0 for >1h. All three are dashboard panels that the R9
   plan already specifies.

## 6. What this doc is not

- Not a release plan. The release gardener owns per-tag rollout; this
  doc is the design that informs it.
- Not a migration script. R4 §7–§8 owns the schema DDL; S6's
  optional narrowing is sketched but not written.
- Not a test plan. Per parent epic, no test work in this epic.
- Not a viewer feature spec. V-R1..V-R4 own the ownership-tab and
  default-filter UX.
- Not a new flag. Reuses `BEADS_OPERATED_BY_ENABLED` from R9.

## 7. Links

- Parent epic: ka-0000003404
- This task: ka-0000006418
- Sibling research:
  - R1 inventory (ka-0000005258) — `operated-by-transitivity-r1-inventory.md`
  - R2 propagation (ka-0000000973) — `operated-by-transitivity-r2-propagation.md` (MR !2014)
  - R3 dibs rule (ka-0000006715, ka-0000003437) — `operated-by-transitivity-r3-dibs.md`
  - R4 audit substrate (ka-0000001481) — `operated-by-transitivity-r4-audit.md`
- R9 rollout flag lineage: `docs/research/human-attribution-rollout.md`
- Flag read site: `kbeads/internal/server/audit.go::operatedByEnabled`
- Helm wiring: `gardenio/helm/values-dev.yaml`,
  `gardenio/helm/gardenio/templates/beads/deployment.yaml`
- Schema: migration `012_human_attribution.up.sql` (operated_by columns +
  partial indexes); R4 will add `operated_by_set_at` + new partial indexes.
