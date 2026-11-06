# Gardener Page v2 — design doc (R1–R6)

**Task:** ka-0000005648 · **Epic:** ka-0000005878 · **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
**Supersedes/extends:** v1 design doc ka-0000003219 (bundle ka-0000002800, epic ka-0000001579)
**Load-bearing bug:** CDQR correctness 6/10 — ka-0000005522
**Builds on:** count-by-`trigger_id` fix (commit `a20290cff`, bug ka-0000007051) — *do not redo it*.

This doc answers the six research questions (R1–R6) the v2 epic must close, with **measured numbers
from prod beads** (taken 2026-06-02), concrete decisions, and a prioritized implementation plan. The
single most important section is **§1 (P0): reconciling the run-count contradiction.** Read that first.

---

## 0. TL;DR — decisions

| # | Question | Decision |
|---|----------|----------|
| **P0** | Header says 439 runs, body shows 13 | **The header counter is the lie, not the body.** Stop advertising `RunsSpawned` as a primary number. Re-label the thread **"Iterations (N rendered)"** distinct from a clearly-secondary **"Engine count: 439 spawned (most pruned)"**. Never imply we can show runs we cannot fetch. |
| **R1** | Run history at scale | Renderable history is **bounded by bead retention, not by our page size** — 86% of the largest gardener's "runs" no longer exist as beads. Keep the daemon-side `trigger_id`/`schedule_id` indexed filter; add **cursor pagination** behind `/runs` only for the genuine tail (≤167 today). No "epoch summary" needed yet — defer until a gardener exceeds ~1k *live* iterations. |
| **R2** | True run-chain | **Fix the producer, then prefer `schedule_key` ordering.** The trigger path stamps no `schedule_prior_agent_id` (bug ka-0000006929, in progress) → chain is unreconstructable for **every** trigger-path gardener today. Land the stamp; meanwhile order by the already-present `schedule_key` (per-fire timestamp) and label fallback **per-ribbon**, not blanket "(by time)". |
| **R3** | Density/accuracy invariants | Encode 4 invariants as golden/accuracy tests: `counter_consistency` (rendered == claimed-renderable), `no_empty_tail` (HasMore ⇒ tail non-empty), `honest_ordering_label` (no "(by time)" when a real chain exists), `bounded_claim` (never advertise a fetchable count we can't deliver). |
| **R4** | Ergonomics | `/gardeners` index + nav link **already shipped**; in-page config edit (`/gardener/{id}/config`) **already shipped**. Remaining: top-nav discoverability, owner-scoped gardener list on `/u/{slug}`. Coordinate with ka-0000004574. |
| **R5** | Liveness | Extend the `/agents/events` SSE filter to match on `trigger_id`/`schedule_id` so an in-flight iteration row updates live; render live rows with an "in progress" badge and no uptime/MR resolution. |
| **R6** | Ownership completeness | Owner resolves for **100%** of sampled gardeners via the 3-tier fallback (`user_bead_id` → `slack_user_id` → bead `operated_by`/display), but **mostly via tier 3** — specs rarely carry `user_bead_id`. Backfill `user_bead_id`/`slack_user_id` onto gardener specs; list a user's gardeners on `/u/{slug}`. |

---

## 1. P0 — Reconcile the run-count contradiction (R1 + R2 root cause)

### 1.1 What the operator sees

On `/gardener/ka-0000003848` (the CDQR target, *"customer-poc AWS POC implementation"*):

- **Header:** `439 spawned / 413 completed`.
- **Life-thread:** 13 rendered cards, numbered Run #1..#13, every ribbon stamped **"Run #N (by time)"**.
- **Lazy `/runs` tail:** the literal string `"No older runs."` (35 bytes), no show-more.

The three numbers contradict each other and the page actively misleads: an operator reasonably
concludes the page is broken or that 426 runs vanished silently.

### 1.2 What is actually true (measured)

I queried prod beads directly (`kd list --type=agent --field trigger_id=<cap>` and `--field
schedule_id=<cap>`, union deduped) for this gardener:

| Source | Count |
|--------|-------|
| `RunsSpawned` (engine snapshot in `gardener` spec) | **439** |
| `RunsCompleted` | 413 |
| Agent beads with `trigger_id == cap` | 60 |
| Agent beads with `schedule_id == cap` | 13 |
| **Union (findable iteration beads)** | **60** |
| **Unfindable gap** | **379 (86%)** |
| `created_at` span of the 60 findable | 2026-05-25 00:19 → 2026-05-26 13:57 |

**The 426 "missing" runs are not a pagination bug — those agent beads no longer exist.** Agent beads
are ephemeral and get pruned; the engine's `RunsSpawned` counter is a monotonic lifetime tally that
keeps counting long after the underlying beads are gone. There is **no query, no cursor, and no index**
that can resurrect 439 cards, because 86% of the data was deleted. Any design that promises "paginate to
all 439 runs" is impossible to satisfy.

This reframes R1 entirely: **the question is not "how do we render hundreds of runs" — it is "how do we
stop claiming a number we can never honor."**

### 1.3 Decision: Iterations (rendered) is the truth; the engine count is a labeled secondary

1. **Primary counter = live renderable iteration count** (`len(iters)` — already what `gardenerView.IterationCount`
   holds after `a20290cff`). Label it **"Iterations"**, not "Runs". This number always equals the number of
   cards the operator can actually click into. It can never contradict the body.

2. **Engine lifetime count becomes an explicit, subordinate annotation:**
   `Engine count: 439 spawned / 413 completed (older runs pruned)`. Rendered small, beneath the
   Iterations chip, clearly framed as a lifetime tally whose beads may no longer exist. The existing
   `reconcileCounters()` / `CounterDrift` plumbing (gardener.go:128) is the right home — but it must flip
   from "subtle drift note" to an **honest, always-shown** secondary when `RunsSpawned >> IterationCount`,
   with the word **pruned** so the operator understands *why* the body is shorter. Today drift only renders
   past a tolerance of 1; for a 379-run gap it should read as a deliberate, explained difference, not a
   bug-shaped warning.

3. **Kill "No older runs." as a dead-end.** When `HasMoreRuns` is false because there genuinely are no
   more *findable* beads (not because pagination failed), the tail should say nothing or render an
   explanatory line — never a bare 35-byte string that reads like an error. See §3 invariant `no_empty_tail`.

4. **Stop calling iteration cards "Run #N".** "Run #N" implies a position in the 439-run spawn sequence,
   which we cannot honor (#1..#13 understates by 34×). Either number cards by their **true** spawned-run
   sequence (only possible once the chain is fixed — §2) or relabel them **"Iteration N (of N shown)"**.
   Until §2 lands, use "Iteration N".

### 1.4 Why this is P0

Every other improvement (nicer cards, live updates, owner chips) is wasted while the headline number lies.
The fix is almost entirely **labeling + honest reconciliation** — cheap, no schema change, no producer
change — and it immediately raises CDQR Correctness because the visible numbers stop contradicting.

---

## 2. R2 — Reconstruct the true run-chain

### 2.1 Measured: the chain is unreconstructable for *every* trigger-path gardener

`orderGardenerIterations` (gardener_timeline.go) walks `schedule_prior_agent_id` back-pointers, falling back
to `created_at` and stamping "(by time)" when the chain breaks. I measured chain integrity across 12 real
gardeners:

| Gardener | Renderable iters | `chainOK` | Why it breaks |
|---------|-----------------:|:---------:|---------------|
| MR Relevance (ka-0000007301) | 167 | ❌ | 167 heads — **no** iteration carries a prior pointer |
| AWS-POC currency (ka-0000005569) | 81 | ❌ | 81 heads |
| LOL release (ka-0000007403) | 77 | ❌ | 77 heads |
| Firehose (ka-0000007400) | 66 | ❌ | 66 heads |
| customer-poc POC (ka-0000003848) | 60 | ❌ | 47 heads, 13 with pointers (partial) |
| Soup-to-nuts GardenerPage (ka-0000003150) | 43 | ❌ | 43 heads |
| Triage-intake (ka-0000003783) | 30 | ❌ | 30 heads |
| trigger-test cov (ka-0000001972) | 1 | ✅ | trivially (single node) |

**Chain-break rate: 11/12 gardeners (92%), and 100% of multi-iteration gardeners.** The cause is precise
and already filed: **the bridge trigger path (`actionSpawnAgent`, triggers.go) never stamps
`schedule_prior_agent_id`** (bug ka-0000006929, in progress, owned by gardenio-gardener-page-r2-kyq). Only
the *legacy scheduler* path stamps it, and almost nothing runs on that path anymore. So every modern
gardener has zero back-pointers → `chainOrder` sees N heads → blanket "(by time)".

`dangling` pointers (a prior referencing a deleted bead) are **0** across the board — the breakage is
purely *absence* of pointers, not corruption.

### 2.2 Decision: fix the producer, prefer `schedule_key`, label per-ribbon

Two layers:

**(a) Producer fix (coordinate with ka-0000006929 — do not duplicate).** In `stampGardenerLoopContext`
(triggers.go:1160) stamp `schedule_prior_agent_id = spec.LastAgentID` *before* `gardenerPostFire` advances
it — exactly as the legacy scheduler path does. This is a one-field write per spawn and restores a true
causal chain going forward. **This work is already in progress;** v2 should consume it, not re-open it.

**(b) Consumer fallback that is honest, not blanket.** The chain only exists *going forward* from the
producer fix; historical iterations will never have pointers. So the page needs a **second ordering key
that already exists today**: trigger iterations carry `schedule_key` — a per-fire timestamp
(e.g. `2026-06-02T07:11`). Measured present on every sampled trigger iteration. Decision:

- Order priority: **(1) `schedule_prior_agent_id` chain** (when intact) → **(2) `schedule_key` ascending**
  → **(3) `created_at` ascending**.
- `schedule_key` is the gardener's own per-fire identity, strictly better than raw `created_at` (which can
  reorder under clock skew / backfill). When we order by `schedule_key` we are NOT guessing "by time" — we
  are using the engine's own fire sequence, so the ribbon should say **"by fire order"**, reserving
  **"(by time)"** strictly for the `created_at` last resort.
- **Label per-ribbon, not per-thread.** `chainOrder` currently returns one `byTime` bool for the whole set,
  so one broken edge stamps every card "(by time)" (CDQR: `bytime_ribbons 13/13`). Change the ordering
  layer to carry an **per-iteration `OrderBasis` enum** (`chain` | `fire-key` | `time`) so a thread that is
  90% chained and 10% inferred labels only the inferred cards.

### 2.3 Numbering

Once (a) lands, number cards by **true chain position** for the chained suffix. For the historical prefix
with no chain, number by `schedule_key` order and mark them inferred. Never present a number that implies a
position in the pruned 439-run sequence (§1.3.4).

---

## 3. R3 — Density & accuracy invariants (golden/accuracy tests)

Coordinate with continuous-improvement epic **ka-0000001553** (the CDQR audit lane) and bug **ka-0000005522**.
Encode these as golden-render + accuracy unit tests (extend `gardener_golden_test.go`,
`gardener_accuracy_invariants_test.go`):

| Invariant | Assertion | Catches |
|-----------|-----------|---------|
| `counter_consistency` | The "Iterations" header number == number of rendered cards. | The 439-vs-13 contradiction (as a *primary* number). |
| `bounded_claim` | The page never offers a "show more / older runs" affordance that resolves to zero, and never advertises a fetchable count larger than the union of findable beads. | "No older runs." dead-end; promising 439 we can't fetch. |
| `no_empty_tail` | `HasMoreRuns == true` ⇒ `/runs` tail renders ≥1 card; `false` ⇒ no dead-end string. | The 35-byte "No older runs." |
| `honest_ordering_label` | No card is stamped "(by time)" when its `OrderBasis == chain`; "(by fire order)" only when `OrderBasis == fire-key`. | Blanket `bytime_ribbons 13/13`. |
| `engine_count_framed` | When `RunsSpawned > IterationCount + tolerance`, the secondary annotation is present AND contains the word "pruned"/"older". | A silent 34× understatement. |

These run on **fixtures derived from the real prod shapes measured above** (a 167-head no-chain gardener; a
60-iter partial-chain gardener with a 379 pruned gap; a clean single-node gardener) so the tests defend the
exact failure modes seen in prod, not synthetic happy paths.

---

## 4. R4 — Ergonomics (reading & writing)

Coordinate with ergonomics research epic **ka-0000004574**. Audit of current code:

- ✅ **`/gardeners` index + filters** — shipped (`gardeners.go`, route `GET /gardeners` registered
  server.go:871; filters all/active/disabled).
- ✅ **In-page config edit** — shipped (`gardener_ops.go`: `GET /gardener/{id}/edit`, `POST /gardener/{id}/config`,
  plus enable/disable/run-now/cancel-run/retire), operator-gated via `viewerCanOperate`.
- ✅ **Create-gardener sheet** — shipped (`GET /create-gardener`).

Remaining R4 work:

1. **Top-nav discoverability.** The origin grievance (ka-0000004574: *"Matt couldn't find the new gardener
   page"*) is a nav problem, not a missing page. Add a **Gardeners** link to the primary nav so `/gardeners`
   is reachable without knowing the URL.
2. **Owner-scoped list on `/u/{slug}`** — see R6.5.
3. **Cross-link from each iteration card** back to the gardener header and to the step bead it worked
   (already partially present per `gardener_crosslinks_test.go`; verify completeness).

These are mostly done; R4 is **not** where the credibility gap lives. Keep it P2.

---

## 5. R5 — Liveness & mixed states

Today `/gardener/{id}` is a static read; an in-flight iteration only updates on full page reload. The
`/agents` index already streams via SSE (`agents_events.go`, topic `agentsIndexEventsTopic`).

**Decision:** extend the agents SSE filter to accept a **single predicate** `trigger_id|schedule_id == capID`
and mount a thin live channel on the gardener page that updates the top (newest) iteration row in place.
Reuse the existing `writeAgentEvent` filter machinery (agent_events.go:150) — it already filters a stream
by a field set; add the gardener key as a filter dimension rather than building a new stream.

Mixed-state rendering rules (already partly handled by `IsLive`):

- A live (non-closed) iteration renders with an **"in progress"** badge, **no** uptime-final / MR-resolution
  (those only exist post-close), and sorts to the top.
- When a new iteration is spawned mid-view, the SSE event prepends a new live row and demotes the prior
  live row to historical — without a reload.

P2: liveness is a polish win, not a correctness fix. Do it after §1–§3.

---

## 6. R6 — Per-user ownership completeness

### 6.1 Measured

The 3-tier owner resolution (`resolveGardenerOwner`, gardener_owner.go: `user_bead_id` → `slack_user_id` →
trigger `operated_by`/display) resolves an owner display for **100%** of sampled gardeners — but the
distribution matters:

- Gardener **specs** almost never carry `user_bead_id` or `slack_user_id` (sampled specs had empty owner
  fields, e.g. MR Relevance ka-0000007301: empty `gardener` owner keys).
- Resolution succeeds because **tier 3** (the trigger bead's `operated_by_display`, e.g. "Matt Baker")
  almost always resolves.
- Iteration beads *do* carry `operated_by: slack:U0OPERATOR1` (measured on MR-Relevance iterations), so
  per-iteration owner chips work; but the **gardener → user_bead** link (needed for the reverse "list a
  user's gardeners" view) is thin.

So: **owner is visible everywhere (good), but the canonical user→gardeners index is under-populated** because
the strong keys (`user_bead_id`/`slack_user_id`) are missing from most specs.

### 6.2 Decisions

1. **Backfill `slack_user_id` / `user_bead_id` onto gardener specs** from the trigger bead's resolved
   operator, so the strong-key path (tiers 1–2) carries the link instead of leaning on tier-3 display
   string matching. One-time migration + stamp-on-create in `create_gardener.go`.
2. **`/u/{slug}` lists that user's gardeners.** Add a "Gardeners" section to the user home, querying
   `type=trigger` beads whose resolved owner == slug (mirrors how `/u/{slug}` already lists agents/work).
3. Surface the owner chip on `/gardeners` index rows and the `/gardener/{id}` header (header already has it
   via `OwnerSlug`/`OwnerDisplay`).

P2, after correctness.

---

## 7. R1 at scale — sizing the real problem

Renderable-iteration distribution across the 12 live gardeners (findable beads, not engine counters):

```
[1, 2, 2, 6, 17, 30, 43, 60, 66, 77, 81, 167]
max = 167   p95 ≈ 81   median ≈ 36
```

- **Largest gardener by *renderable* iterations: 167** (MR Relevance). The 439 figure is a *lifetime spawn
  tally*, 86% of whose beads are pruned.
- The eager page renders 25 (`gardenerEagerRuns`), the lazy tail the rest. At p95=81 and max=167 the current
  `Limit:500` single scan is **comfortably sufficient** — there is no scale crisis in *findable* data today.
- **Growth:** the active gardeners accrue roughly one iteration per scheduled fire (e.g. MR-Relevance fires
  ~half-hourly). A long-lived gardener could exceed 500 *findable* iterations in weeks **only if bead
  retention is extended**; against current pruning, findable counts plateau.

**Decision (R1 information model & pagination):**

- Keep the daemon-side indexed `trigger_id`/`schedule_id` filter (already in place; backed by daemon
  migration 031 partial B-tree index per the v1 doc).
- Add **cursor pagination** behind `/runs` keyed on `(schedule_key, id)` so the tail is stateless and
  bounded per request, replacing the "fetch up to 500 then slice" approach. This future-proofs against
  retention changes without an epoch-summary layer.
- **Defer "summarized epochs"** until a gardener demonstrably exceeds ~1k *findable* iterations. Building an
  epoch-rollup now would optimize a problem the data says we don't have — and would distract from the P0
  honesty fix.

---

## 8. Prioritized implementation plan (children to spin out)

P0 first, smallest-honest-fix first. Each becomes a child task of epic ka-0000005878.

### P0 — Stop the contradiction (label + reconcile; no schema/producer change)
1. **Relabel header: "Iterations (N)" primary; "Engine count: X spawned (older pruned)" secondary.**
   Flip `reconcileCounters`/`CounterDrift` from a tolerance-gated subtle note to an always-shown, *framed*
   secondary when `RunsSpawned >> IterationCount`. (gardener.go, gardener_page.go, header template.)
2. **Kill the "No older runs." dead-end;** render nothing / an explanatory line when there is no findable
   tail. (gardener_page.go `handleGardenerRunsSection`, partial template.)
3. **Rename iteration ribbons "Run #N" → "Iteration N"** until the true chain lands. (gardener_timeline.go
   `runLabel`.)
4. **Golden/accuracy tests** for `counter_consistency`, `bounded_claim`, `no_empty_tail`,
   `engine_count_framed` using prod-shaped fixtures. (R3.)

### P1 — True chain + honest ordering labels
5. **Consume the producer fix ka-0000006929** (stamp `schedule_prior_agent_id` on trigger spawns).
6. **Add `schedule_key` as the second ordering key** + per-ribbon `OrderBasis` enum (`chain`/`fire-key`/`time`);
   replace the whole-thread `byTime` bool. (gardener_timeline.go `chainOrder`/`orderGardenerIterations`.)
7. **`honest_ordering_label` accuracy test.** (R3.)

### P2 — Liveness, ownership, ergonomics, scale
8. **Cursor pagination** behind `/runs` keyed on `(schedule_key, id)`. (R1/§7.)
9. **Gardener-page SSE liveness:** extend agents SSE filter to a `trigger_id|schedule_id` predicate; live
   top-row updates. (R5.)
10. **Owner backfill** `slack_user_id`/`user_bead_id` onto gardener specs + stamp-on-create. (R6.)
11. **`/u/{slug}` "Gardeners" section** + owner chip on `/gardeners` rows. (R6/R4.)
12. **Top-nav "Gardeners" link.** (R4.)

### Already shipped — do NOT re-open
- Count-by-`trigger_id` fix (commit `a20290cff`, ka-0000007051).
- `/gardeners` index + filters; in-page config edit; create-gardener sheet (gardener_ops.go, gardeners.go).

---

## 9. Coordinates / do-not-duplicate

- v1 doc ka-0000003219 (bundle ka-0000002800), v1 epic ka-0000001579.
- CDQR correctness bug ka-0000005522 (P0 fold-in).
- R2 producer fix **in progress**: ka-0000006929 (gardenio-gardener-page-r2-kyq) — consume, don't redo.
- Continuous-improvement / CDQR audit lane: ka-0000001553.
- Ergonomics research epic: ka-0000004574.
- Stop-gate fix that mis-closed v1 gardener: ka-0000008051 / MR 3327.

---

*Measured 2026-06-02 against prod beads via `kd list --type=agent --field {trigger_id,schedule_id}=<cap>`
(union deduped) and `kd show <cap>` gardener-spec counters. Gardener set = the 12 live `type=trigger`
gardeners in prod at time of writing.*
