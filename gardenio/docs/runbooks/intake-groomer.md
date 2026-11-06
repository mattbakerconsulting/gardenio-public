# Loop Runbook: Gardener-Intake Groomer

| | |
|---|---|
| Trigger bead | ka-0000003841 (`trigger:schedule`; successor to closed ka-0000006210) |
| Anchor epic | ka-0000003517 (intake-groomer lane; complement of the sweep-IN janitor ka-0000003783 / [`triage-intake-janitor.md`](./triage-intake-janitor.md)). **Never groom it** — see [Hard skips](#hard-skips--never-groomable-never-a-strike) |
| Cadence | hourly (`33 * * * *`), cooldown 5m, max_concurrent 1; pre-flight fires only while the gardenio `needs-intake` lane holds an *actionable* (unblocked, ungroomed) bead |
| Role | gardener |

> **Dormancy gate (why it stays asleep on a triaged lane).** The pre-flight
> `bead_query` is project-scoped to gardenio (ka-0000003939), scans
> `"statuses": ["open","blocked"]` (bug ka-0000004035 — see below), and declares
> `"exclude_blocked_by_open_types": ["escalation","decision","task","bug"]`
> (ka-0000007887). So a groomable bead that is `blocked_by` an OPEN
> escalation, decision, task, or bug is subtracted from the count — a groomer
> cannot de-lane a bead whose blocker is still open regardless of the blocker's
> type, so those beads must not wake it. Conversely a **stale-`blocked`** bead
> (`status=blocked` but ZERO open blocker edges — bug ka-0000004035) is NOT
> subtracted: the `blocked` status alone no longer hides it from the gate, so it
> wakes the groomer exactly like a ready open bead. Before the `blocked` status
> was added to the gate, 14 such beads sat invisible to BOTH this lane and
> gardener-bugfix for up to ~4 months (a dead-letter queue). When every bead in the lane is either
> blocked or already triaged, the effective count is 0 and the gardener stays
> dormant instead of waking a pod every 30m to re-confirm there is nothing to
> do. The filter is **fail-safe and count-reducing only**: it never drops a
> bead (excluded beads keep their `needs-intake` label and re-count the instant
> their blocker closes), and any uncertainty — or a matched set above the
> 500-row scan cap — keeps the full count so the gardener errs toward waking.
> See `docs/triggers.md` §Externally-blocked exclusion for the full semantics.

## 1. Condition to keep true

The gardenio `needs-intake` lane drains: real gardenio work beads swept into the
lane by the triage-intake janitor (ka-0000003783) get triaged, scoped,
ready-gated, and **de-laned** into the normal ready queue (or closed as junk)
faster than the janitor sweeps new ones in. Dispatchers — including the
ayce-eater throughput governor — treat `needs-intake` as do-not-dispatch, so an
ungroomed lane stalls the whole fleet.

## 2. Drift signal

- The groomable backlog grows cycle-over-cycle. KPI query. `groomable` is the
  UNION of two captures — `status=open` **and** the stale-`blocked` subset of
  `status=blocked` (bug ka-0000004035). Both label predicates MUST sit inside
  ONE `select()` — the older `select(A) and (B)` form is malformed jq that
  counts thread beads too and miscounts the lane:

  ```sh
  # Shared in-lane filter (project:gardenio, NOT thread/slack). Reused by both captures.
  FILT='[.[]|select(((.labels|join(","))|test("project:gardenio")) and
        (((.labels|join(","))|test("thread|slack-thread|source:slack|source:multi|source:schedule"))|not))]'

  open_groomable=$(kd list --all-projects -l needs-intake --status=open \
      --type task --type bug --type feature --type epic --limit 5000 --json \
    | jq "$FILT | length")

  # STALE-BLOCKED capture (bug ka-0000004035). `status=blocked` beads were NEVER
  # fetched by this lane (status=open-only), so 14 needs-intake beads with ZERO open
  # blocker edges sat structurally invisible to BOTH the groomer and gardener-bugfix
  # (which excludes needs-intake by charter) — a dead-letter queue up to ~4 months
  # deep. `status=blocked` is stamped at filing and never cleared; no lane consumes
  # it. `--with-blockers` is authoritative and does the whole job in ONE capture
  # (advice ka-0000002706): per-row `open_blockers[]` is the daemon's blocker verdict
  # (edge types blocks/blocked-by/depends-on in status open/in_progress/blocked/
  # deferred — parent-child is hierarchy, NOT a wait edge). An EMPTY open_blockers[]
  # is a groomable candidate, treated exactly like a --no-blockers open bead; a
  # non-empty one is genuinely blocked and stays in lane (BLOCKED verdict). Do NOT
  # re-resolve edges client-side — the daemon already computed the verdict here.
  stale_blocked=$(kd list --all-projects -l needs-intake --status=blocked \
      --type task --type bug --type feature --type epic --with-blockers --limit 5000 --json \
    | jq "$FILT | [.[]|select(((.open_blockers//[])|length)==0)] | length")

  groomable=$((open_groomable + stale_blocked))   # KPI groomable_start / groomable_end
  ```

  > **The explicit `--type` list is load-bearing — do not drop it (bug
  > ka-0000006847).** `kd list` injects a hidden `kind=issue` filter *only when
  > no `--type`/`--kind`/`--all-types` is passed* (`kbeads/cmd/kd/list.go`
  > `implicitIssueKind`). The pre-flight gate (ka-0000003841 `conditions`) sets
  > `types:[task,bug,feature,epic]` and **no** kind, so it scans `kind=data`
  > work beads too (a `type=task`/`kind=data` bead is gate-visible but
  > `kind=issue`-invisible). Left as a bare `-l needs-intake --status=open`, the
  > KPI would force `kind=issue` and read a KPI-invisible `kind=data` groom as
  > Δ0 on a genuinely productive cycle — indistinguishable from a failed cycle.
  > ka-0000003300 (a `type=task`/`kind=data` needs-intake bead) sat unmeasurable
  > in the lane ~6 weeks for exactly this reason. Passing the same
  > `task,bug,feature,epic` type list the gate uses suppresses the injection so
  > the KPI spans the **same rows** the gate scans. The `exclude_labels`
  > (`source:multi|source:schedule|thread|slack-thread|source:slack`) likewise
  > mirror the gate's `exclude_labels` so the two counts agree.
  >
  > **Scope decision — widen the KPI, do NOT narrow the gate.** The gate
  > deliberately omits a `kind=issue` clause: narrowing `evalBeadQuery`
  > (`gardenio/controller/internal/scheduler/conditions/leaf_daemon.go`) to
  > `kind=issue` would have left ka-0000003300 — the lane's only actionable bead
  > — undiscovered forever, defeating the point of the wake gate. The gate seeing
  > *more* rows than a naive KPI is the safe direction; the fix is to lift the
  > KPI up to the gate, which this query does.

- A bead you **verified** de-laned this cycle reappears carrying
  `needs-intake` at end-of-cycle. There are **two** distinct re-lane sources,
  and telling them apart is the whole diagnosis:
  - **Sweep-IN janitor re-sweep** — a `triage-intake:` (or similar) comment is
    added when the bead comes back. The janitor's skip-list keys on a recent
    `intake-groomed:` comment (§5), so this path is normally suppressed for a
    bead you just de-laned; when it fires it **always leaves a comment**, which
    is the tell.
  - **Controller OrphanSweeper re-lane** — the OrphanSweeper
    (`gardenio/controller/internal/scheduler/orphan_sweep.go`) re-adds the
    `needs-intake` label **in code**. Since ka-0000007945 it leaves an
    **`orphan-sweep: re-laned needs-intake`** comment **when — and only when — it
    re-surfaces a bead that carries a PRIOR de-lane** (`DeLanedAt` set); a
    first-time orphan surfacing is still comment-free. So a de-laned bead that
    the sweeper reclaims after the cooldown lapses now DOES carry a comment (the
    `ReLaneMarker` prefix), which is the auditable tell for exactly the
    groomer↔sweeper case this section is about (historically this path left NO
    comment — bug ka-0000007706 / escalation ka-0000007479). Its `isOrphan()`
    exempts a bead that HAS A PARENT
    or HAS OPEN CHILDREN, and honors a de-lane **cooldown**
    (`defaultOrphanDeLaneCooldown = 4h`, `orphan_sweep.go:69`). The cooldown
    only DELAYS the reclaim: a **parentless**, unclaimed, childless-or-drained
    de-laned bead is re-laned on the first sweep tick (every 5m,
    `defaultOrphanSweepInterval`) AFTER the 4h cooldown lapses — silently (no
    comment). If the groomer then re-de-lanes it without parenting, the cooldown
    resets and the cycle repeats every ~4h, forever. The permanent fix is to
    give the bead a parent (step 1 of the 4-step de-lane) so `isOrphan()` skips
    it outright.
    - **Cooldown history (do not reintroduce 30m).** The cooldown was originally
      **30m == the groomer's `3,33 * * * *` cron**, which phase-locked the two
      into a permanent re-lane ping-pong for any parentless drained epic. Fix
      ka-0000008193 (landed 2026-07-27, commit 25770af69) **raised it to 4h**
      (~8× the 30-min cadence) so the periods cannot phase-lock;
      `TestDeLaneCooldownExceedsGroomerCadence` pins the margin. Earlier notes
      citing a "30m cooldown" predate this fix — the live value is 4h.
  - **Third actor (historical) — the news+ready relevance gardener.** A THIRD
    party used to re-apply `needs-intake`: the News+Ready Relevance Critic
    gardener (trigger ka-0000005840), which held the opposite policy about
    parentless ready rows and re-laned beads this loop had de-laned — including
    ka-0000004837, whose 3 OPEN children already exempt it from the
    OrphanSweeper's `HasOpenChildren` guard, so neither documented source could
    explain its loop. Resolved by decision ka-0000000290 (`anchor-parent-wins`,
    2026-08-24): parenting under ka-0000002583 is now the fleet-wide answer for
    parentless-ready beads, and that gardener was amended to SKIP
    ka-0000002583-parented beads. Its trigger is now closed. Kept here because
    the diagnostic lesson outlives the actor: **a reappearance the two
    documented sources cannot explain means a third actor, not a mystery** —
    look for another loop holding a contradictory policy about the same label
    before concluding the bead is cursed.
  - **Diagnosis:** read the re-lane COMMENT PREFIX, not the presence/absence of
    a comment. A reappearance carrying an **`orphan-sweep: re-laned`** comment ⇒
    OrphanSweeper reclaiming a bead it had a prior de-lane for, and it was
    parentless — corroborate via the trigger-engine / controller logs
    (`orphan-sweep: surfaced orphaned bead for intake` and `orphan-sweep:
    completed surfaced=… candidates=…`). A reappearance carrying a
    `triage-intake:`-style comment ⇒ janitor. A reappearance with NO new comment
    at all ⇒ a **first-time** OrphanSweeper surfacing (no prior de-lane), which
    stays silent by design. A reappearance within one 4h window is a genuine new
    loop (parenting the bead was skipped), not the fixed resonance bug.
- Cycles ending with zero de-lanes, zero closes, and a non-empty groomable
  backlog — failed cycles.

## 3. Restoring evidence

A de-lane is only real once the exact-match verify **exits 0**; count VERIFIED
evidence, not aggregate deltas:

- Per bead: `kd show <id> --json | jq -e '(.labels // []) | index("needs-intake") | not'`
  exited 0 this cycle → count as ready-gated. Closed junk counts separately.
- Do NOT infer success from `groomable_end − groomable_start`: the sweep-IN
  janitor runs concurrently and adds beads while you groom, so the count can
  rise on a fully successful cycle.
- "Could not read state" is NEVER treated as "label absent" — the verify fails
  safe (non-zero on daemon error / empty response / bad id).

## 4. Autonomous-vs-approval boundary

Autonomous:
- De-lane ready beads; close junk/dup beads (with a one-line reason comment);
  add scope-gap / blocked comments; parent obvious orphans under an epic.
- Bulk de-lane at scale (operator-approved 2026-07-13, decision
  ka-0000006411): the old ~8/cycle cap is REMOVED for beads passing the
  safe-bulk filter.

Requires a human (`gb decision`, at most ONE fight-loop decision per cycle):
- An observed janitor↔groomer fight-loop (see RE-SWEPT branch below).
- Genuinely ambiguous scope where only the operator can decide
  priority/parentage (file `gb decision create --no-wait`, continue other
  beads).

Never touch:
- Beads labeled `thread`, `slack-thread`, or `source:slack` — they belong to
  human thread agents; leave them in the lane.
- The ayce-eater mission epic (ka-0000002885) or any
  `type=trigger`/`type=schedule`/`type=agent` bead — they carry `needs-intake`
  as a side-effect, not as work.
- No MRs, no merges, no code, no releases. Triage + scope + ready-gate ONLY.

### Hard skips — never groomable, never a strike

Two classes of bead are **out of scope by construction**. Do not de-lane them,
do not count `intake-groomed:` strikes against them, and do not file a
fight-loop decision about them — a bead that was never in scope cannot be in a
fight-loop, so the §RE-SWEPT circuit breaker must not fire on it. Both rules
are permanent, from operator-resolved decision ka-0000004440 (`exempt-anchor`,
2026-08-24).

- **OWN-ANCHOR — ka-0000003517 (this loop's own anchor epic).** It is the
  intake-groomer lane's anchor (the row at the top of this runbook): the
  `needs-intake` label on it is **correct by construction**, marking the lane
  itself, not un-triaged work. Five de-lane attempts (2026-07-10 .. 07-12) each
  "failed" because the label was never wrong. Skip it silently; it is not a
  grooming failure and never earns a strike. This is the ONE bead where a
  reappearing label means the system is working.
- **ON-ICE — any bead carrying the `on-ice` label.** `on-ice` means a human
  DELIBERATELY parked it. Grooming an `on-ice` bead is the mistake — it is not
  a candidate whose third reappearance is suspicious, it is a bead you should
  never have touched. Skip it entirely, every cycle, regardless of parentage,
  strike count, or how ready it looks. (ka-0000001177 "Production non-PII
  read-only access for gardenio" is the instance that surfaced this: two grooms,
  both wrong.) Leave the `needs-intake` label alone — de-laning an `on-ice`
  bead would push deliberately-parked work into the ready queue.

Filter both out at the **walk** step, before any per-bead verdict, so they
never reach the READY/BLOCKED/RE-SWEPT branches and never consume the
one-decision-per-cycle budget.

## 5. Durable state

- The `needs-intake` label itself is the lane membership.
- `intake-groomed:` comments are LOAD-BEARING, not documentation: the sweep-IN
  janitor's skip-list keys on an `intake-groomed:` comment newer than 60m to
  avoid re-sweeping a bead just de-laned. (Confirmed at scale 2026-07-13: a
  bulk pass that skipped comments had 58/610 revert — all parentless orphans.)
- `intake-groom FAILED:` comments record unverified removals (no duplicates
  cycle-over-cycle).
- `intake-blocked:` comments record the blocker set the groomer last observed
  on a bead left in-lane as BLOCKED. They are the dedup baseline for the
  BLOCKED branch: re-comment only when the current open-blocker set differs
  from the id(s) named in the newest such comment (no duplicates
  cycle-over-cycle). Suppressing the no-change comment does not lose the
  signal — the bead is still reported in the wrap-up handoff.
- Fight-loop decisions are the circuit breaker for looping beads.
- No cycle-wrap beads — the summary lives ONLY on `gb stop --wrapup` and
  `gb squawk`; wrap beads leak as forever-open ready-queue pollution.

## THE ONE THING YOU MUST NOT GET WRONG

**De-lane means REMOVE THE LABEL, not comment about it.** A bead only leaves
the lane when `needs-intake` is actually GONE. Posting a "releasing to queue"
comment while the label is still on the bead is a NO-OP: the bead never enters
`gb ready` and no dispatcher can pick it up — the exact failure that wedged the
fleet for 8 straight cycles (bug ka-0000005322). Every de-lane =
remove → VERIFY the label is gone → then comment. Never comment first, never
comment without verifying.

### The canonical 4-step de-lane (mandatory for EVERY de-lane, in ANY branch)

1. **PARENT it first if it is parentless.** Run
   `kd dep list <id> | grep -c parent-child`. If the count is **0**, add a
   parent BEFORE touching the label:
   `kd dep add <id> ka-0000002583 --type parent-child`
   (ka-0000002583 — anchor epic "gardenio fleet-infra root epics
   (anti-orphan-sweep parent)"), then confirm the edge landed
   (`kd dep list <id> | grep -c parent-child` now returns ≥1) before step 2.
   This is **not optional** and it is the ONLY permanent fix for the
   OrphanSweeper re-lane loop: a parentless, unclaimed, de-laned work bead is
   re-surfaced by the controller OrphanSweeper
   (`gardenio/controller/internal/scheduler/orphan_sweep.go` `isOrphan()`) on the
   first sweep tick after the de-lane cooldown lapses
   (`defaultOrphanDeLaneCooldown`, 4h — see §Drift signal), silently and
   forever. The cooldown only DELAYS the reclaim; a parent edge (or an open
   child) is what makes `isOrphan()` skip the bead outright. A bead that already
   has a parent (count ≥1), is itself a tree-root with OPEN children
   (`HasOpenChildren` guard), or carries a release-policy label is already
   sweep-exempt — the count check or the guard skips it, so no parenting is
   needed.
2. **REMOVE the label — and CLEAR a stale `blocked` status.** Run exactly:
   `kd label remove <id> needs-intake`.
   Do NOT substitute a bare `kd update <id>` — with no `--remove-label` flag
   that is a NO-OP. (If you must use kd update it MUST read
   `kd update <id> --remove-label needs-intake`.)
   THEN, if the bead's `status` is `blocked` while its daemon-computed
   `open_blockers[]` is EMPTY (a stale-`blocked` dead-letter bead — bug
   ka-0000004035), clear the status too so `gb ready` and every dispatcher can
   actually see it: `kd update <id> --status open`. `status=blocked` was stamped
   at filing and never cleared, and **no other lane consumes that status** — the
   intake groomer owns this hygiene, exactly as it owns removing the label. A
   bead whose `open_blockers[]` is NON-empty is genuinely blocked: it keeps its
   status and stays in lane per the BLOCKED verdict — do NOT de-lane it here.
3. **VERIFY (fail-safe, exact-match)** — run exactly:
   `kd show <id> --json | jq -e '(.labels // []) | index("needs-intake") | not'`
   Read the EXIT CODE, not the printed text. Exit 0 = confirmed gone → step 4.
   Non-zero = not verified: retry step 2 once (using the
   `kd update <id> --remove-label needs-intake` form), re-verify. Still
   non-zero → STOP for this bead: no comment, not counted as ready-gated;
   record in handoff as unverified; add a diagnostic
   `intake-groom FAILED: … — observed labels: <paste .labels>` comment only if
   the bead does not already carry a recent one.
4. **THEN document** — only after step 3 exited 0:
   `kd comment add <id> "intake-groomed: scoped + ready-gated, releasing to queue"`.

## Cycle playbook

1. **Snapshot KPI**: record `groomable_start` (KPI query above). Also
   `gb news --active-only --ids-only` → ALIVE set.
2. **Walk each groomable gardenio needs-intake work bead** (type
   task/bug/feature/epic; NOT thread/slack; NOT trigger/schedule/agent; NOT a
   §Hard-skip bead — drop ka-0000003517 and anything labeled `on-ice` here,
   before assigning a verdict).
   Scope by **type, not kind** — a work-typed bead grooms even when its `kind`
   is `data` rather than `issue` (bug ka-0000006847; the gate scans these, so a
   walk that silently drops them under-grooms the lane). The §2 KPI query's
   explicit `--type` list already spans them; when walking ad hoc, pass the same
   `--type task --type bug --type feature --type epic` (or `--all-types` then
   filter) so `kd list` does not inject the hidden `kind=issue` clause.
   Scope by **blocker EDGE, not the status field** — the candidate set is the
   §2 union of `status=open` AND stale-`blocked` (`status=blocked` with an EMPTY
   daemon `open_blockers[]` — bug ka-0000004035). Read the verdict off
   `open_blockers[]` from a `--with-blockers` fetch, NOT off `status`: a
   stale-`blocked` bead is groomed exactly like a ready open one (and its status
   is cleared in de-lane step 2), while an `open`-status bead carrying a live
   open blocker edge still gets the BLOCKED verdict.
   For each, decide ONE of:
   - **READY** — title/description/acceptance/type clear, EMPTY `open_blockers[]`
     (no live blocker edge, regardless of whether `status` reads `open` or a
     stale `blocked`), unowned → de-lane via the 4-step procedure
     (parent-if-parentless is step 1; a stale `blocked` status is cleared in
     step 2).
   - **DESIGN/SCOPE-GAP** — needs a design doc first: stays in lane; comment
     naming exactly what's missing. Do NOT dispatch code crew (not this lane);
     if a design bead is flagged do-not-auto-crew, leave it and say so.
   - **BLOCKED** — NON-empty `open_blockers[]`, i.e. a live open blocker edge
     (depends-on/blocks/operator escalation) — NOT merely a `blocked` status:
     leave in lane (and do NOT clear the status), but **comment ONLY when the blocker set has actually
     CHANGED** — a blocker added, removed, or now-closed — since the newest
     groomer-authored `intake-blocked:` comment on the bead, **or when the bead
     carries no `intake-blocked:` comment yet**. When you do comment, use the
     stable prefix and name the current open blocker id(s) so the next cycle
     has a baseline to diff against:
     `kd comment add <id> "intake-blocked: still BLOCKED by <id[,id...]>, staying in lane"`.
     Otherwise the verdict is unchanged — **skip the comment silently** and
     account for the bead in the cycle wrap-up handoff only (§Wrap-up). Judge
     "changed" by comparing the blocker id(s) named in that newest
     `intake-blocked:` comment against the bead's current open blockers: an
     identical open set is NOT a change; anything else is. (A pre-existing
     legacy groomer comment that already names the blocker id — e.g. the old
     `still correctly BLOCKED by <id>` line — counts as a valid baseline too,
     so a lane already carrying one does not earn a fresh comment on the first
     post-fix cycle.) This mirrors the
     no-duplicates-cycle-over-cycle rule §5 already applies to
     `intake-groom FAILED:` comments — a bead blocked on OPERATOR action
     (sign-off, IAM grant, canary drive) will not unblock on a groomer
     timescale, so it must not accumulate one near-identical "still BLOCKED"
     line per 30m cycle (bug ka-0000004217).
   - **JUNK/DUP** — slack-mention spam, duplicate, stale noise: one-line
     reason comment, then `kd close <id>`.
   - **STALLED-IN-PROGRESS artifact** — a reaper-reopened
     "stalled in_progress: …" bead: verify against `gb news`; if real and
     unowned, de-lane via the 4-step procedure; if already resolved, close it.
   - **RE-SWEPT (fight-loop)** — fires ONLY on OBSERVED loop evidence, never on
     parentlessness alone. Check comment history.

     > **Loop evidence is YOUR OWN comments, not a janitor comment
     > (ka-0000008193).** The re-laner in prod is the controller **OrphanSweeper**
     > (`internal/scheduler/orphan_sweep.go`), NOT the sweep-IN janitor (whose
     > trigger bead ka-0000003783 is CLOSED). The OrphanSweeper re-adds the label
     > in code and leaves **NO comment**, so a rule keyed on a `triage-intake:`
     > re-sweep comment can never fire for the path actually doing the re-laning.
     > Key loop detection on the count of YOUR OWN prior `intake-groomed:`
     > comments instead — those you always leave, so they are reliable evidence
     > regardless of whether any janitor comment exists.
     - *First-time groom* (no prior `intake-groomed:` comment): not a
       fight-loop even if parentless. De-lane via the **4-step procedure** —
       which now REQUIRES parenting a parentless bead under ka-0000002583 (step
       1) BEFORE removing the label, so the OrphanSweeper cannot reclaim it. This
       is no longer optional: a parentless de-lane loops silently every ~4h by
       design (§Drift signal). No decision.
     - *Observed loop (once)* (the label is present again AND you find ONE prior
       `intake-groomed:` comment of your own — i.e. you already de-laned it a
       previous cycle and it came back): de-lane once more, then file AT MOST ONE
       fight-loop decision for the whole cycle (`gb decision create --no-wait`,
       options: give-it-a-parent / janitor-skip-rule / leave-in-lane). A
       `triage-intake:` re-sweep comment dated after your `intake-groomed:` is
       also loop evidence when present, but do NOT require it — the OrphanSweeper
       leaves none. Additional looping beads go in handoff only.
     - *Third strike (hard stop)* (TWO+ prior `intake-groomed:` comments of your
       own with the label present again, or an open fight-loop decision
       referencing the bead): do NOT de-lane a third time. Leave the label, skip,
       note in handoff. The decision is the circuit breaker; once one is open,
       stop grooming that bead until it resolves.

       > **Rule out a hard skip BEFORE counting strikes (ka-0000004440).** A
       > §Hard-skip bead — this loop's own anchor ka-0000003517, or anything
       > labeled `on-ice` — accumulates `intake-groomed:` comments and keeps
       > "reappearing" for the trivial reason that the label was never wrong and
       > the bead was never groomable. That is not a fight-loop; filing a
       > decision on it burns the cycle's single decision on a non-question. The
       > §Hard-skips filter at the walk step is what prevents this, and the
       > 2026-08-24 fight-loop decision existed only because it was missing:
       > of its three beads, TWO (ka-0000003517, ka-0000001177) were out of
       > scope and only ka-0000004837 was a real loop. Before you file, confirm
       > the bead is genuinely groomable.

     > **The class-level fix landed (ka-0000008193):** the OrphanSweeper de-lane
     > cooldown was raised from 30m (== the groomer cron, which phase-locked the
     > loop) to 4h, so a de-lane now suppresses re-laning for ~8 groomer cycles.
     > A bead that STILL reappears after that fix is a genuine new fight-loop, not
     > the resonance bug — treat it with the strikes above.
3. **Scale — SAFE-BULK de-lane** (when the lane exceeds ~50 groomable beads;
   prefer P1 first, then P2/P3/P4):
   a. TWO server-side queries, no N+1 dep-list loop (the explicit `--type` list
      keeps `kd list` from injecting the hidden `kind=issue` filter, so
      `kind=data` work beads the gate sees are swept in too — bug ka-0000006847):
      `kd list --all-projects -l needs-intake --status=open --type task --type bug --type feature --type epic --no-blockers --limit 5000 --json`
      (`--no-blockers` excludes blocked work and hides cron fire-artifacts), PLUS
      the stale-`blocked` capture (bug ka-0000004035) — `status=blocked` beads the
      old `status=open`-only query never saw:
      `kd list --all-projects -l needs-intake --status=blocked --type task --type bug --type feature --type epic --with-blockers --limit 5000 --json`
      then keep only rows whose daemon-computed `open_blockers[]` is EMPTY
      (`jq '[.[]|select(((.open_blockers//[])|length)==0)]'`). The empty-
      `open_blockers[]` predicate and `--no-blockers` apply the SAME daemon
      blocker-edge semantics, so the two candidate streams merge into one and the
      per-bead 4-step de-lane (which now clears the stale `blocked` status, step 2)
      applies uniformly to both.
   b. Keep only: `project:gardenio`, NOT thread/slack, NOT
      `source:multi`/`source:schedule`, NOT type trigger/schedule/agent, NOT
      ka-0000002885, NOT a §Hard-skip bead (`on-ice`, or ka-0000003517), and
      unassigned. Prefer non-empty descriptions (>40c) on
      the first pass; title-only beads on a second pass (de-lane is
      reversible).
   c. For EACH kept id, run the full 4-step de-lane **including the parent-if-
      parentless step 1 and the comment** — even in bulk mode (the parent edge
      is what stops the OrphanSweeper re-lane, and the janitor skip-list depends
      on the comment). Terse batch form: parent parentless ids under
      ka-0000002583, then
      `kd comment add <id> "intake-groomed: bulk ready-gated, releasing to queue"`.
   d. **CANARY FIRST**: before a large batch, de-lane ONE parented bead and
      re-check 60s later that the label stayed gone (guards the OrphanSweeper
      revert regression). After the batch, spot-check ~10 random de-lanes; if
      any reverted, STOP and re-file the revert escalation.

      > **What actually stops the revert (ka-0000008193).** `ORPHAN_SWEEP_ENABLED`
      > is NOT set anywhere in `gardenio/helm/`, and it defaults to `true`
      > (`envBoolDefault("ORPHAN_SWEEP_ENABLED", true)`), so the sweep is LIVE in
      > prod — the old "mitigated via `ORPHAN_SWEEP_ENABLED=false`" claim was
      > never in effect. What holds the sweep off a bead you just de-laned is the
      > in-code de-lane **cooldown** (`defaultOrphanDeLaneCooldown`, 4h): every
      > label removal records a durable `beads.label.removed` event that the
      > sweeper reads, so a de-lane suppresses re-laning for 4h whether or not
      > you leave a comment. The 4h backoff is deliberately ~8× the 30-min
      > groomer cadence: when it was 30m (== the cron) the two phase-locked into
      > a permanent re-lane loop for any parentless bead whose children were all
      > closed (a drained epic — the one case the HasParent/HasOpenChildren
      > guards don't cover). If you WANT the sweep fully off, set
      > `ORPHAN_SWEEP_ENABLED=false` in the chart — do not assume it already is.

   For small lanes (≤50 groomable) the per-bead walk with individual comments
   is fine.

## Wrap-up contract — KPI delta mandatory

Every `gb stop --wrapup` reports:

- `tldr`: one-line outcome summary (≤500 chars) — **validator-required**; the
  wrap-up is rejected without it.
- `accomplishments`: one line per bead touched — id + verb
  (ready-gated / closed-junk / scoped).
- `kpi`: `groomable_needs_intake: <start> -> <end> (Δ±N). closed: <N>.
  ready-gated: <N>.` — with `groomable_start`, `groomable_end`,
  verified-de-laned, closed, and failed/unverified as **distinct numbers**.
  `groomable_start`/`groomable_end` count only the rows the §2 KPI query spans
  (`type` in `task,bug,feature,epic`, any kind). A groom you **verified** with
  the per-bead exact-match check (§3) counts as ready-gated **even if the bead
  is invisible to the count** — e.g. a `kind=data` work bead the gate saw but a
  bare `kind=issue` KPI would miss (bug ka-0000006847). Report such a groom
  under `ready-gated`/verified-de-laned, never inferred from the `groomable_*`
  delta. If your KPI query omitted the `--type` list (older runbook form), say
  so: the delta then reflects `kind=issue` beads only and is not a full picture.
- `handoff`: beads still stuck + why; decisions filed. **This MUST include
  every BLOCKED bead left in-lane this cycle — including those whose blocker
  set was unchanged and therefore got NO new comment** (BLOCKED branch of the
  cycle playbook). Suppressing the duplicate comment moves the signal here, it
  does not drop it: list each such bead id + its current open blocker(s) so the
  blocked residue stays visible cycle-over-cycle.
- `progress_score` ≥1 on any cycle that de-laned or closed ≥1 bead.

Sanity check before wrapping: count verified evidence, not the aggregate delta
(the janitor adds beads concurrently). Flag a re-sweep ONLY if a bead you
personally verified de-laned this cycle reappears with `needs-intake`.

## Lifecycle

Standing sweeper: park between sweeps (`gb stop`), wake on the cron. Do NOT
self-complete on a momentarily-empty lane — empty groomable backlog means
park, not stop (dormancy is automatic via the bead-query pre-flight). Target:
each cycle either de-lanes ≥1 bead, closes ≥1 junk bead, or states explicitly
that the groomable backlog is 0.

## Provenance

Migrated from the inline prompt of trigger ka-0000006210 on 2026-07-19
(task ka-0000007116, epic ka-0000007152, W3 gardener-runbooks workstream).
The trigger prompt is now a pointer to this file; scheduling mechanics stay on
the trigger bead.
