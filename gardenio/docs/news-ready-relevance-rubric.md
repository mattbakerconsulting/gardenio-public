# `gb news` + `gb ready` Relevance Rubric

> **Status:** v1 (2026-06-23). Authored under epic [[ka-0000004960]] in
> response to a Slack request from Matt Baker (thread [[ka-0000004401]]):
> *"new gardener and rubric for this channel, pretty self explanatory: gb
> news and ready relevance critic/fix loop"*.
>
> This rubric defines **what a relevant, trustworthy `gb news` and `gb ready`
> queue looks like**, names the **violation classes** that erode that
> relevance, and prescribes the **fix action** for each. It is the rubric the
> [news+ready relevance gardener](#the-gardener) applies on a cadence: read the
> two queues, score them, fix the violations it is authorized to fix, and
> surface genuine forks to the operator.

## 0. Why this rubric exists

`gb news` and `gb ready` are the two queues an agent reads at the *start of
every session* to orient itself:

- **`gb ready`** answers *"what should I work on?"* — open, unblocked,
  unclaimed issue beads, priority-sorted.
- **`gb news`** answers *"what is everyone else doing, and what just
  changed?"* — in-progress work by others, stale zombies, recently closed
  beads, resolved decisions, and open decisions awaiting the operator.

These queues are **load-bearing**: every agent's first decision of every run
is made from them. When they fill with noise — cron-spawned per-run beads,
zombie in-progress beads whose agent is long gone, mislabeled or unparented
work, duplicate decisions — agents waste wake-ups, claim dead work, or miss
the genuinely-ready P0. **Queue relevance is an availability property of the
fleet, not a cosmetic one.**

This rubric is *strict*: each gate is a binary pass/fail, each failure maps to
a named violation class, and each class has a single prescribed fix. No
averaging, no "vibes" score — a queue either passes a gate or it files a fix.

## 1. Scope & vocabulary

- A **queue** is the rendered output of one command for one project:
  `gb news --project <p>` or `gb ready --project <p>`.
- A **row** is one bead line in a queue.
- **Relevant** = a row that a reading agent would *act on* or *needs to know
  about right now*. A row that no agent would ever claim, or that misrepresents
  live state, is **noise**.
- **Stale / zombie** = an `in_progress` bead whose assignee agent is no longer
  live (no running pod; terminal/Failed/Succeeded phase). `gb news` already
  separates these into the "Stale (agent gone, N)" section — the rubric's job
  is to drive that N toward 0, not just display it.
- **Per-run cron bead** = a `kind=issue` task bead auto-created by the trigger
  scheduler for a single gardener firing (titles shaped like
  `[1:1:<ts>] Multi[cron]: Gardenio gardener-… `). These are *operational
  exhaust*, not claimable work, but because they are `kind=issue` they are
  **not** caught by `gb ready`'s `ExcludeScheduled` filter (which only drops
  `type=schedule`). They are the dominant `gb ready` noise source.
- **Ground truth** for "is this agent live?" is the same signal `gb news`
  uses: the active-agent set derived from running pods
  (`gardenio/controller/cmd/gb/news.go`, `splitActiveStale` +
  `activeAgents`). The gardener must use `gb news` / `kd` data, never guess.

Every fix the gardener applies must be **reversible and logged**: a `kd comment`
recording before→after, or a close with a reason. The gardener never silently
mutates a bead.

## 2. The strict gates

A queue **passes** only if it satisfies every gate below for every row. A
single violating row does not fail the whole queue — it files the prescribed
fix for *that row*. The queue's **relevance score** (§4) is the fraction of
rows that are clean.

### `gb ready` gates (RDY)

| Gate | Statement | How it's checked | Violation class | Fix |
|---|---|---|---|---|
| **RDY-1 No-cron-exhaust** | No per-run cron/gardener-firing bead appears in ready. | Title matches `^\[\d+:\d+:` or contains `Multi[cron]:` / `gardener-delivery` / `gardener-release-owner` per-run shape, OR `fields.spawned_by_trigger` set. | `CRON-EXHAUST` | Close the per-run bead if its run is over (agent gone + no open MR); else label it so it's filtered. File a bug if the scheduler is leaking these into `kind=issue` (root-cause fix belongs in the trigger engine, not whack-a-mole). |
| **RDY-2 Claimable** | Every row is genuinely claimable: open, unblocked, unassigned (or assignee gone). | Cross-check `status=open`, no open `blocks` deps, assignee empty or agent not live. | `UNCLAIMABLE` | If blocked → it should not be in ready (file bug on query); if assigned to a live agent → it belongs in news, not ready. |
| **RDY-3 Parented-or-labeled** | Every task/bug is parented by an epic OR carries a clear `project:` + topical label (advice ka-0000005995). | Check `parent` edge and labels. **SKIP any row parented under anchor epic `ka-0000002583` — it is compliant by construction (§5.6).** | `ORPHAN-READY` | Reparent to the right epic, or file a comment proposing parentage; never leave a bare orphan task at P0. **Never hand-apply `needs-intake` as the fix — see §5.6.** |
| **RDY-4 Priority-honest** | P0/P1 rows are genuinely urgent; the top of ready is not stale high-priority cruft outranking real work. | Inspect the top N by priority: is each a real, current ask? | `PRIORITY-INFLATION` | Re-prioritize down (with a logged comment) stale/aspirational P0s so live work surfaces. |
| **RDY-5 Title-legible** | A reader can tell what the bead is from its title in <5s; no opaque hashes or truncated machine titles as the whole signal. | Title review. | `UNREADABLE-TITLE` | Comment proposing a human title; for agent-generated beads, fix at the generator. |
| **RDY-6 Parent-alive** | Every row has at least one **open** parent-child parent. RDY-3 asks only *"has a parent"* — it never asks whether that parent is still alive, which is why this class passed every gate for seven consecutive `1.00` cycles. | Resolve **every** parent-child edge, not `parent_id` — see §4.6. A row is a violation only when **all** its parents are closed. | `DEAD-PARENT` | Per-row disposal, never a blanket action: **close-as-moot** or **reparent to a live epic** (§5.7). Never leave it dangling — dangling is what produced the class. |

### `gb news` gates (NWS)

| Gate | Statement | How it's checked | Violation class | Fix |
|---|---|---|---|---|
| **NWS-1 Stale-drained** | The "Stale (agent gone)" section trends toward 0; no zombie older than the staleness budget (default 1h, advice ka-0000007726) survives a cycle un-triaged. | For each stale row: age since `updated_at`, no live pod, no open decision. | `ZOMBIE` | If work is done → close. If unfinished and reclaimable → unassign (return to ready) or respawn. If genuinely abandoned → close with reason. Stale > 1h with no live agent and no open decision is **up for grabs** (advice ka-0000007726). |
| **NWS-2 In-progress-is-live** | Every row in "In-progress by others" has a live agent actually working it. | Confirm assignee in active-agent set. | `FAKE-INPROGRESS` | Same as ZOMBIE — a non-live in-progress bead must move to stale handling, not masquerade as active work. |
| **NWS-3 No-duplicate-decisions** | Open decisions awaiting the operator are deduplicated; sibling gardeners do not each file the same merge-gate. | Group open decisions by prompt/epic; flag near-duplicates. | `DUP-DECISION` | Cancel/merge the duplicates (advice: news already dup-groups these; the gardener closes the redundant ones with a pointer to the canonical). |
| **NWS-4 Closed-section-signal** | The "Closed in last window" section reflects real progress, not cron-bead churn inflating the count. | Count what fraction of recently-closed are per-run cron beads vs real work. | `CLOSED-NOISE` | Root-cause via RDY-1's bug; do not hand-close, but note the inflation in the scorecard. |
| **NWS-5 Decision-freshness** | No open operator decision is stale (>4h, not recently reflagged). | Age open decisions. | `STALE-DECISION` | Reflag it (`gb decision reflag <id> --reason '...'` bumps priority and reposts to Slack) or, if the decision is moot, cancel it with a reason. |
| **NWS-6 Decision-respected** | No bead this cycle mutated (close/unassign/reparent/reprioritize) while an open operator decision already covered it. | Before each mutation, run BOTH §5.0 legs — `kd list --type=decision --status=open --mentions <bead-id> --json` AND `kd search <bead-id> -t decision -s open --json` (leg 2 is the only one that sees `fields.context`, bug ka-0000001507; its `-t`/`-s` filters must be server-side or the gate reads a 20-row page and fails open, bug ka-0000007862); after the cycle, none of the mutated ids carried an open decision. | `PREEMPTED-DECISION` | **Do not mutate** — the row is PARKED (§5.0). Record it in the scorecard as `deferred (decision ka-X)`. If a mutation already happened, it is a scored defect on this loop: disclose it on the decision with a reversal command and count it against the cycle. |
| **NWS-7 Not-a-decision** | Every open operator decision is a *genuine fork*: its options name **materially different next actions**, and standing fleet policy does not already answer it. Operator rule (ka-0000003001, 2026-08-24): *"if you cannot name what materially changes per option, it is not a decision."* A fleet-internal item covered by standing policy must not reach the operator at all. | Enumerate every open decision (`kd count`/`--limit 200`, never a page — §4.2). For each, read `fields.options` (§4.7): does any pair of options lead to the **same** concrete next action (ack/done/continue, or two labels that describe the same operator click)? Does standing policy / an existing advice already dictate the answer? | `NOT-A-DECISION` | Cancel it with a reason pointing at the answering policy (`gb decision cancel <id> --reason …`) — this is a `DUP-DECISION`-class autonomous fix (§5.1), NOT an operator-gated cancel, because a non-fork is noise by definition. File a bug on the **generator** if the same shape recurs (see NWS-8). |
| **NWS-8 Fix-the-generator** | No lane files the *same* decision on a schedule. A lane with **≥2 open decisions sharing a prompt prefix** is a GENERATOR filing repeatedly, not N independent forks. Operator rule (ka-0000003001): *"file against the generator, never sweep the rows."* Precedent: three 2026-08 generators (weekday standup cross-post, nightly directive-count watch, two gardeners with contradictory needs-intake policies) were retired by editing the generator, not cancelling instances. | Group all open decisions by normalized prompt prefix (first ~40 chars of `prompt`, or filing agent/gardener — §4.7). Any group of size ≥2 is a violation. | `DECISION-GENERATOR` | Do **not** sweep the instances. File ONE bug under epic ka-0000000903 naming the generator (the trigger/gardener/prompt that emits the shape) and the fix (retire/edit the generator), link every instance, and cancel the redundant instances only after the generator is filed. Changing another gardener's config is the meta-gardener's domain (§5.1) — report the symptom, defer the tuning. |

## 3. Grading

Each queue gets a letter grade from its clean-row fraction (§4) plus hard
short-circuits:

| Grade | Condition |
|---|---|
| **A** | ≥ 95% rows clean, 0 zombies > 1h, 0 dup decisions. |
| **B** | ≥ 85% rows clean, ≤ 2 zombies, 0 dup decisions. |
| **C** | ≥ 70% rows clean OR 3–6 zombies. |
| **D** | 50–69% clean OR a CRON-EXHAUST flood (> 5 per-run beads in ready). |
| **F** | < 50% clean — the queue is more noise than signal; an agent reading it is actively misled. |

**Short-circuits (apply first):**

- **CRON-FLOOD** — if `gb ready` shows > 5 per-run cron beads, grade is at
  most **D** regardless of other rows, and the gardener MUST file/track the
  root-cause bug (the scheduler leaking `kind=issue` per-run beads into the
  ready query), not just close them one-by-one.
- **ZOMBIE-PILE** — if `gb news` stale section has > 10 entries, grade is at
  most **D** and stale triage is the cycle's top priority.

## 4. The relevance score

```
clean_rows  = rows passing every applicable gate
total_rows  = rows rendered in the queue (per section)
relevance   = clean_rows / total_rows           # 0.0 – 1.0
```

Report `relevance` per queue (`ready`, `news`) and a combined fleet figure.
The gardener's job over time is to drive both toward 1.0 and keep them there —
a queue that was A last cycle and is C this cycle is a regression worth a
squawk.

**Parked rows are `deferred` — neither clean nor a violation.** A row the §5.0
gate finds parked behind an open decision is excluded from *both* `clean_rows`
and `total_rows`, and listed separately in the scorecard with its decision id.
Counting it clean hides real noise; counting it a violation pressures the next
cycle into "fixing" what the operator already owns — which is the exact failure
§5.0 exists to prevent.

**`PREEMPTED-DECISION` (NWS-6) is scored against the CYCLE, not the queue.** It
never moves a queue's relevance figure, because it is a defect in the loop's
conduct rather than in what the queue displays. A cycle can grade `ready`
A / 1.00 and still be a defective cycle — which is precisely what happened on
2026-08-20 23:43. Report the count, the decision id, and the exact reversal
command in its own scorecard section; any non-zero count is a regression
regardless of how both queues graded.

### 4.1 A grade is a snapshot, not a state — check the deployed build

**A clean queue whose defenses are un-deployed will regress, and the grade
will not warn you.** Measured 2026-08-20: cycle 14:13 scored `ready` **A /
1.00**; cycle 14:43 — thirty minutes later — read **F / 0.67**, with 19 of the
20 page-1 rows a deliverable bead. Nothing about the queue had rotted. A
release had landed *between the two cycles* carrying a feature and not its
backstop: 2026.232.15 shipped the `artifact`-category rule + migration 043
(ka-0000007274) while the `gb ready` type-exclusion backstop (ka-0000006718)
was still only on `main`. Deployed binary, new filter, ~9.4k unswept legacy
rows, no backstop.

So when a queue regresses, **suspect the build before you suspect the beads**:

```sh
gb version                                    # note the commit
git fetch origin
git merge-base --is-ancestor <fix-commit> <build-commit> \
  && echo "fix IS deployed" || echo "fix NOT deployed"
```

One `--is-ancestor` check turned "why is `ready` full of dashboards again"
into a five-minute root-cause fix. Skipping it invites the whack-a-mole
§5 exists to forbid — closing symptom rows one cycle at a time while the
generator keeps running.

Corollary for **which fix to reach for**: a flood of *legitimate* beads that
merely do not belong in a claimable queue is not a closing problem. Re-derive
the field that excludes them (here `kd admin backfill-category`) and the rows
leave `ready` while staying intact and findable. Prefer the root-cause sweep
over any per-bead mutation, and verify afterward that no real work-type row
left the queue with them (diff the pre/post id sets; confirm
`task`/`bug`/`feature`/`epic` picked up no `category=artifact`).

### 4.2 Count with `kd count`, never with `jq length` on a `kd list` page

**`kd list` returns a 20-row PAGE by default, and `jq length` on it looks
exactly like a total.** This loop scored its own queues off a page-1 undercount
for at least three consecutive cycles: scorecards ka-0000006618, ka-0000007277
and ka-0000006593 each reported *"20 open gardenio decisions"* and graded NWS-3 /
NWS-5 against that figure. The true count on 2026-08-20 15:13 was **74** —
`kd count --type=decision --status=open -l project:gardenio` = 74, and
`kd list … --limit 200` returns 74 rows. The `20` was never a measurement; it
was the page size, indistinguishable from a total in the output.

The cost is not cosmetic — it is a **false clean**. NWS-3 dedup over page 1
found max multiplicity 1 ("nothing to cancel"); the same dedup over all 74
surfaced a 4-deep cluster of open decisions stacked on one live gardener trigger
(ka-0000001007), which is a *generator* of queue noise and was invisible from
page 1. A gate scored over a truncated set reports PASS for the rows it never
fetched.

So for any denominator that feeds a grade:

```sh
kd count --type=decision --status=open -l project:gardenio   # the total
kd list  … --limit 200 --json                               # the rows to scan
```

`gb ready --json` is not subject to this (it returns the full ready set — 227
rows on 2026-08-20, while `gb ready`'s *rendered* output shows 20 with a
`20 beads (251 total)` footer). Score page-1 separately and say so: page 1 is
what an agent reads first, so "19/20 of page 1 was noise" is a legitimate and
important sub-measurement — it is just not the queue's relevance score. Report
both, and never let a page size stand in for a total.

### 4.3 `updated_at` is not a "last touched" timestamp — comments do not bump it

**`created_at == updated_at` does NOT mean a bead is untouched.** Comment
activity is invisible to `updated_at`, so any "inert row" / "nobody has looked at
this in N days" metric derived from it is measuring field-mutation only, not
attention.

Measured 2026-08-20 16:13 (this loop, caught pre-filing): a scan of the 225-row
`gb ready` set found **183 rows (81%) with `created_at == updated_at`**, which
reads as an alarming "81% of the queue is abandoned". The figure was spurious.
The tell was a row created *the same day* — `ka-0000004790`, age ~2h, carrying a
comment — sitting in the "never touched" bucket. The direct probe on
`ka-0000002750`:

```sh
kd show <id> --json | jq -r .updated_at   # 2026-07-14T12:52:51Z
kd comment add <id> "…"                   # comment 182891 added
kd show <id> --json | jq -r .updated_at   # 2026-07-14T12:52:51Z  (UNCHANGED)
```

So before any age/staleness gate is scored from a timestamp, **probe what that
timestamp actually responds to** on a bead you control. A plausible-looking
denominator built on an untested field assumption produces a confident wrong
grade — the same failure mode as §4.2's page-size-as-total, and it would have
been filed as a queue defect rather than the measurement bug it was.

Note this cuts both ways: `updated_at` remains valid for what it *does* track
(status/priority/assignee/field writes). It is a **mutation** clock, not an
**activity** clock. For real activity you need comment timestamps
(`kd show <id>` renders them) or the bead's event history — not `updated_at`.

### 4.4 When a detector already owns the shape, read its logs before closing anything

**A stale row whose cleanup some component already owns is a broken-detector
report, not a bead to close.** Before applying any §5 fix, ask: *is there code
whose job this was?* If yes, the finding is why that code did not fire — and
closing the row by hand destroys the only evidence while leaving the generator
running.

Measured 2026-08-20 20:13: four open `gitlab_mr_merge` escalations had an
already-**merged** MR and a **closed** requesting task (ka-0000002678 !7588,
ka-0000004159 !7485, ka-0000000781 !7482, ka-0000000277 !7429), aged 1–4 days.
Closing four beads was the obvious cleanup. It would also have been the whole
mistake: the escalation **review-flag** detector
(`bridge/escalation_review_flag.go`) exists to handle exactly this shape, so the
real question was why it hadn't. The bridge log answered in one grep —

```sh
kubectl logs -n gardenio-system deploy/gardenio-gitlab-bridge --since=30m \
  | grep -iE "escalation.*(reap|review-flag)"
```

— **252 failures in 6h** across 7 beads: `mr_landed: unknown field`. The five
`mr_landed*` fields were never declared on `type:escalation`, so the feature had
been 100% dead for the 21 days since it shipped (bug ka-0000004864). Hand-closing
would have scored a clean cycle and left the outage running.

Two properties made it invisible, and both generalize:

- **It failed at ERROR and retried forever.** A loop that logs loudly and never
  gives up produces no queue-visible symptom except the rows it fails to clean.
  Absence of a fix is not absence of a fixer.
- **The guard test for this exact class stayed green.**
  `TestBuiltinEscalationReflagPingbackFieldsDeclared` was written to catch
  unknown-field 400s on escalation writers, and its own doc comment warns that
  adding a writer without adding the field "reintroduces the re-surface 400" —
  which is what happened. So a passing test is not evidence the path works;
  only the live log or a live probe is.

Practical rule for this loop: for any violation class a component owns
(CRON-EXHAUST → stale-reaper, moot escalations → escalation reaper/review-flag,
CLOSED-NOISE → trigger engine), grep that component's logs **before** the first
`kd close`. A detector failing silently is a P1 root cause; the rows it left
behind are just its symptom, and §5's ROOT-CAUSE clause already prefers the bug
over the batch. Corroborating signal is cheap and worth capturing: two sibling
escalations self-reaped correctly mid-cycle (`signal=mr_state`), which is what
localized the fault to one detector rather than the whole reaper.

### 4.5 A full page is not a result set — check the detector's OWN query for a cap

§4.2 says never let a page size stand in for a total *in your own queries*. The
same trap exists one layer down, and it is nastier: **the detector you are
auditing has its own query, with its own cap, and a truncated result there looks
exactly like a healthy steady state.**

Measured 2026-08-20 20:43. Seven parentless tasks sat in `gb ready` with no
`needs-intake` label — beads the orphan sweep exists to label. Every signal said
the sweep was fine: the workload was up, `orphan-sweep: completed` appeared
regularly, and it had genuinely labeled three beads in the last 6h. Each run
logged `"candidates":1,"surfaced":1` — a plausible quiet queue.

Running the sweep's own query settled it in one call:

```sh
# what ListReady actually fetches (main.go:924, orphanReadyLimit = 1000)
curl -s ".../v1/beads?limit=1000&no_open_deps=true&status=open&type=task,bug,feature,epic" | jq length
# => 1000   ← exactly the cap: the tell

curl -s ".../v1/beads?limit=5000&..." | jq length
# => 4779
```

The sweep saw **1000 of 4779** eligible beads — 79% of its input invisible.
Worse, the result is ordered **newest-first**, so the window spanned only
`2026-08-20` back to `2026-07-05`, and the 7 orphans sat at positions 1649–2200:
permanently unreachable, no matter how many times the sweep ran. Bug
ka-0000000077.

The generalizable part is **why `candidates:1` was not evidence of health**: it
counts candidates *within the truncated window*, which only ever holds beads new
enough to survive the newest-1000 cut. The sweep looked like it was keeping up
with a quiet queue; it was keeping up with the last six weeks and structurally
blind before that. A detector's success counter is scoped to what it fetched —
it can never report the rows it never saw.

So when a detector is provably alive (per §4.4) yet its shape keeps appearing:

- **Read its query, not just its logs.** Find the limit constant and run the
  query yourself at that limit and well above it. `returned == limit` is a
  truncation until proven otherwise.
- **Check the ordering.** A cap plus recency ordering is not a slow backlog, it
  is a permanent dead zone with a sharp age boundary. Ask which rows can *never*
  be reached, not merely which were missed this run.
- **Distrust per-run counters as coverage.** `surfaced/candidates` measures work
  done inside the window; only comparing against the untruncated total measures
  coverage.

Same cycle, same trap, this loop's own scoring: RDY-3 graded **PASS / 0
parentless ready rows** the prior cycle (ka-0000007661) off a `--no-parent` sweep
capped at `--limit 200` against a true `kd count` of **320**. Reproduced exactly
— `--limit 200` → 0 orphans; `--limit 1000` → 7. Whenever a gate reports a clean
zero, re-run its query at a higher limit before believing it: a false clean and a
real one are byte-identical in the output.

### 4.6 Scoring RDY-6: `parent_id` is a scalar over a non-singleton set

**Never score RDY-6 off `parent_id`.** A bead can carry several parent-child
edges, and in this queue that is common, not exotic — 47 of 167 rows (28%) were
multi-parent when bug ka-0000001227 was filed. `parent_id` is one arbitrarily
chosen edge, so "the parent shown is closed" is **not** the same claim as "this
bead has no live parent."

Measured this cycle (2026-08-24, 177 ready rows): **56 rows displayed a closed
parent, but only 32 were genuinely dead-parented.** The other **24 each had an
open EPIC parent** on the same bead that `parent_id` never surfaced. Scoring off
the displayed column would have overstated the class by 75% — and, worse, the
"obvious" disposal (close it, the parent is done) would have closed 24 rows of
live work sitting under a live epic.

So the gate resolves every edge:

```sh
gb ready --json > /tmp/ready.json                    # full queue, not the 20-row render (§4.2)
# for each row: every UPWARD parent-child edge, then each parent's status
kd dep list <row-id> --json | jq -r '.[] | select(.type=="parent-child" and .bead_id=="<row-id>") | .depends_on_id'
kd show <parent-id> --json | jq -r '.status'
```

Three traps worth naming, all hit live while building this gate:

- **`kd dep list --json` has its own key vocabulary** — `bead_id`,
  `depends_on_id`, `type`, and **no** `depends_on_status` / `depends_on_title`
  (those exist on `kd show`'s `dependencies[]`, not here). A wrong guess is a
  silent `null`, not an error (advice ka-0000000767), which reads as "no closed
  parents" — a false clean. Resolve each parent's status with its own
  `kd show`.
- **The direction filter is mandatory.** `kd dep list` returns edges in both
  directions; without `select(.bead_id == <row>)` a row's own CHILDREN are
  counted as parents.
- **`jq length` on a `kd list` page is not a count** (§4.2). Enumerating
  candidate reparent targets with `kd list --type=epic --status=open --json |
  jq length` returned **20**; `kd count` on the same filter returned **173**. The
  paged answer makes a live successor epic look nonexistent, which converts a
  reparent into a wrong close.

### 4.7 Scoring NWS-7 / NWS-8 and the two decision success-measures each cycle

The operator (ka-0000003001, 2026-08-24) requires **both counts reported every
cycle alongside NWS-5**, plus two lagging success-measures from the plan. Raw
open-decision count is a *lagging* indicator — it fell 76→49 on the operator's
own drain sprint with **no generator change** — so the measures below are what
tell you whether the *generators* are actually being fixed, not merely the wall
being swept.

Fetch every open decision once, then project many times (§4.2, advice
ka-0000002706):

```sh
S=$(mktemp -d)
kd count --type=decision --status=open -l project:gardenio            # the DENOMINATOR — never a page
kd list  --type=decision --status=open -l project:gardenio --limit 500 --json > "$S/dec.json"

# NWS-7 candidate — options whose labels collapse to <2 distinct next actions.
# .fields.options is an ARRAY in --json output, but a STRING on some rows and
# absent on others — normalize all three before counting distinct labels.
jq -r '.[] | (.fields.options | if type=="string" then fromjson else . end) as $o
       | select(($o|type)!="array" or ($o | map(.short // .label) | unique | length) < 2)
       | .id' "$S/dec.json"

# NWS-8 candidate — lanes with >=2 open decisions sharing a prompt prefix
jq -r '.[] | [(.fields.prompt // "")[0:40], .id] | @tsv' "$S/dec.json" \
  | sort | awk -F'\t' '{c[$1]++; ids[$1]=ids[$1]" "$2} END{for(k in c) if(c[k]>1) print c[k]"\t"k"\t"ids[k]}'
```

`fields.options` is a JSON string (note the `fromjson`); a decision with no
options blob is itself an NWS-7 candidate. Both jq programs are *candidate
filters, not verdicts* — read each flagged decision and confirm the options
genuinely collapse / the prefix genuinely marks one generator before cancelling
or filing.

**Report these measures in every scorecard** (NWS-5 plus the two counts and the
two success-measures the operator named), targets in brackets:

| measure | how | target |
|---|---|---|
| `NWS-5` open decisions stale >4h | `kd list … --json`, age from `created_at`/last reflag | trend ↓ |
| `not_a_decision` (NWS-7) | jq above, verified | **0** |
| `decision_generators` (NWS-8) | jq above, verified | **0** |
| `unowned_decisions` | open decisions with empty `assignee` | **0** (49 on 2026-08-24) |
| `dead_handle_decisions` | open decisions whose `--requesting-task` / referenced subject is closed AND not a live fork (§ the ka-0000003001 analysis) | **0** (10 on 2026-08-24) |

`unowned_decisions` and `dead_handle_decisions` are the plan's two SUCCESS
MEASURES: a decision nobody owns and a decision whose re-entry handle is dead
are both dead weight in the wall no matter how the raw count moves. **File a bug
(under ka-0000000903) on any NWS-7 or NWS-8 violation** — a non-fork or a
generator is a defect in what reaches the operator, exactly the class this task
(ka-0000006588) exists to make measured rather than remembered.

## 5. Fix authority (what the gardener may do on its own vs. must escalate)

### 5.0 Pre-mutation gate — is this row already parked behind an open decision?

**Before ANY autonomous mutation of a bead — close, unassign, reparent,
re-prioritize — check whether an open operator decision already covers it.**

**Run BOTH legs.** `--mentions` alone fails open (bug ka-0000001507):

```sh
# leg 1 — the reference index (title/description/notes/comments)
kd list --type=decision --status=open --mentions <bead-id> --json
# leg 2 — full-text, the ONLY leg that sees fields.context
kd search <bead-id> -t decision -s open --json
```

**Leg 2 MUST use the server-side `-t decision -s open` filters** — never
`kd search <bead-id> --json | jq 'select(.type=="decision" …)'`. That older
form fetches a **20-row page** and filters it client-side, so it is the §4.2
defect *inside the gate*, and it fails OPEN. Measured 2026-08-21 (bug
ka-0000007862), prod build 2026.233.4: two parked beads read CLEAN under the
paged form — `ka-0000005862` (parked by `ka-0000007931`, target at result
depth 75) and `ka-0000007417` (parked by `ka-0000002315`, depth >200, found
only at `--limit 500` of a 475-row set). The filtered form returns **1 row**
for each. On a 50-bead sample it scored **50/50 recall** against text ground
truth versus 48/50 for the paged form — and it is strictly cheaper, since the
server discards the non-matches instead of shipping 475 rows to be thrown
away. `kd search` does not inject the `kind=issue` default, so `kind=data`
decisions are eligible without any extra flag.

If **either** leg returns **non-empty**, the row is **PARKED**: do **not** mutate it.

Why two legs: the `bead_references` index that backs `--mentions` extracts ids
from a fixed set of buckets — `title`, `description`, `notes`, `comment:<id>`,
and (since the `extractFieldsRefs` fix, bug ka-0000007416) `fields`
(`kbeads/internal/store/postgres/queries_references.go`). Before that fix leg 1
could not see `fields.context`, which is where `gb decision create` puts the
operator-facing body while leaving `description` empty — measured 2026-08-21,
**73/73** open gardenio decisions had an empty `description` and **71/73**
carried their body in `fields.context`.

**Both legs currently measure 100%.** Re-measured 2026-08-21 07:50Z against a
full text ground-truth scan (all **117** open gardenio decisions × all **178**
`gb ready` rows → **62** genuinely parked rows):

| surface | recall |
|---|---|
| leg 1 (`--mentions`) | **62/62 (100%)** |
| leg 2 (`kd search -t -s`) | **62/62 (100%)** |
| `gb ready` `[parked: ka-X]` badge | **62/62 (100%)** |

The write-side hook is confirmed both in code and empirically:
`replaceBeadReferences` writes a `fields` bucket on every INSERT/UPDATE, and a
bead created *after* the backfill indexes an id that appears **only** in its
fields blob.

**This does not retire leg 2 — it is the reason to keep it.** Leg 1 measured
**9%** recall at 06:43Z on 2026-08-21 and **100%** an hour later; the only
thing that changed was a backfill (ka-0000001352). The index is only as fresh
as the last write hook or backfill, so a healthy reading is a *snapshot*, not a
guarantee. Leg 2 is full-text and does not depend on that index at all.

**Re-measure each cycle rather than trusting this table.** Compute the parked
set two ways — the index (leg 1) and a client-side `contains()` scan over every
open decision's full JSON — and compare. A leg-1 decay from 100% means the
write-side hook has regressed, which is a *new* bug distinct from the backfill
gap closed on 2026-08-21.

Even two legs are **best-effort, not sound**: residual misses can live in a
decision's `options` blob. Treat a gate miss as possible, and prefer the durable
fix in ka-0000001507 (index `fields.context` as a reference source-field bucket,
making `--mentions` authoritative).
Record it in the scorecard as `deferred (decision ka-X)`, not as a violation to
fix. The operator already owns the fork; touching the bead executes one leg of a
question they have not yet answered and silently removes their choice.

This gate is cheap — one call per mutation candidate, and §5's batch cap already
holds the candidate set to ≤10/cycle — and it closes a real gap the rest of §2–§5
does not: **every other check in this section inspects the row's OWN state**
(target closed? agent live? open MR on the branch?). None of them asks *"has a
prior cycle — or a sibling gardener — already escalated this exact bead to the
operator?"* The relevance loop's whole job is mutating beads, so it sits exactly
in that gap.

> **How this class got here (2026-08-20, bug ka-0000007469).** Cycle 22:43
> correctly declined to reap 10 moot `tune:` rows and escalated the fork as
> decision **ka-0000003987** (*revive the lane, reap the rows, or both?*). Cycle
> 23:43 independently re-derived that the same rows were moot, judged the close
> authorized under §5, and closed 8 of them — which is verbatim the `reap` leg of
> the still-open decision. The failure was **not** a misreading: the gardener
> followed §5 and §5 produced the wrong action, because nothing in §2–§5 told it
> to look for the decision first. Two facts made it near-inevitable: (1) the
> corpus already has this check for a *different* object — the MR-review dedup
> rule (`kd list --type=decision --status=open --mentions <id>` before *filing or
> dispatching*), but scoped to decisions, not to *mutating beads*; and (2)
> prior-cycle escalations are invisible in the loop's own inputs — neither
> `gb ready` nor `gb news` marks a row as "parked behind an open decision", so
> the parked rows rendered as ordinary claimable work.

Note the scope difference from §5's *"Cancelling a non-duplicate operator
decision"* rule below: that rule governs touching the **decision**; this gate
governs touching the **bead the decision is about**. Both are the operator's to
resolve; neither is autonomously actionable.

### 5.1 Autonomous vs. escalate

The gardener has **bounded autonomous authority**, mirroring the meta-gardener's
guardrail (advice kd-... / `gardener-meta-scoring-prompt.md`):

**May fix autonomously (logged, reversible):**

- Close a per-run cron bead whose run is over (`CRON-EXHAUST`, agent gone, no
  open MR) — with a close reason.
- Close / unassign a zombie `in_progress` bead per NWS-1 (advice ka-0000007726
  explicitly makes stale > 1h up for grabs).
- Reparent an orphan ready task to its obvious epic (`ORPHAN-READY`).
- Cancel a duplicate open decision pointing at the canonical (`DUP-DECISION`).
- Re-prioritize an obviously-stale P0 down a band, with a comment
  (`PRIORITY-INFLATION`).
- **File** bugs/tasks for root-cause fixes (the scheduler leak, a bad title
  generator) under this epic — **and stop at filing.** The gardener's fix
  authority over its own instrument ends at the bead. Filing lets normal
  dispatch assign the fix to a crew agent, so the in_progress/assignee interlock
  the whole fleet dedups against actually applies. **The gardener must NOT open
  an MR or push a fix branch for a bug it filed** unless it first
  `kd claim`s that bead — an unclaimed push is invisible to every dedup in the
  fleet, because the bead's assignee still points at whoever dispatch gave it
  to. (Bug ka-0000007898: the 05:13 cycle filed ka-0000007416 *and* implemented
  it in MR !7625 without claiming, while dispatch independently spawned the
  assigned crew onto MR !7626 — two authors, same file, same latent flake,
  neither aware of the other.)

**Never (outside `kd claim` first):**

- Implement a bug the gardener filed. Filing is authorized; implementing is not.
  If the gardener genuinely believes it must implement (e.g. a one-line fix it is
  uniquely positioned to make), it **`kd claim`s the bead first** so the
  assignee interlock is real, then follows the §5.1.1 pre-push dedup gate below.

### 5.1.1 Pre-push dedup gate (before opening ANY MR for a bead)

Before opening — or pushing a fix branch that will become — an MR for bead `X`,
even one the gardener has claimed, run BOTH checks. If either says another author
already owns the work, **do not push**; file/append a comment on `X` pointing at
the existing owner and stop.

```sh
# 1 — does X have a live assignee that is NOT this gardener?
kd show <X> --json | jq -r '.assignee'      # non-empty and != you => someone owns it
# 2 — does an open MR already name X? (glab mr list defaults to open)
glab -R example-org/gardenio mr list --search <X>
```

This mirrors the MR-review dedup rule the corpus already applies before *filing
or dispatching* a review, extended to the *implement* path. Cheap (two calls),
and it is the exact interlock that was missing when the loop raced its assigned
crew.

**Must escalate via `gb decision` (never auto-apply):**

- Closing a bead whose status is ambiguous (might still be live, might have an
  in-flight MR) — propose close vs keep.
- Cancelling an operator decision that is *not* an obvious duplicate.
- Any change to another gardener's config (cron/cooldown/triggers) — that is
  the **meta-gardener's** domain; this gardener reports the symptom and defers.
- Any destructive batch action affecting > N beads in one cycle (default N=10)
  — propose the batch, list the beads, `gb yield`.

**Standing guardrail:** this gardener owns *queue relevance*, not *work
prioritization policy* and not *other gardeners' tuning*. When a fix would
change what work the fleet does (vs. how cleanly the queue reflects it), it
escalates.

### 5.6 Anchor epic ka-0000002583 is compliant — never re-lane it, never hand-apply `needs-intake`

**Two hard rules, both settled by operator decision ka-0000000290 (Matt Baker,
2026-08-24, chosen option `anchor-parent-wins`):**

1. **A row parented under anchor epic `ka-0000002583` PASSES RDY-3.** Skip it.
   It is not an orphan, not a violation, and not a candidate for any parentage
   fix. Anchor-parenting is the **fleet-wide** answer for parentless-ready
   beads, not one gardener's local convention.
2. **`needs-intake` is never this gardener's fix for anything.** The label is
   owned end-to-end by code — the orphan sweep applies it
   (`gardenio/controller/internal/scheduler/orphan_sweep.go`), the auto-intake
   sweep strips it (`auto_intake_sweep.go`), and the two are disjoint by
   construction on a `HasParent` discriminator. Hand-applying it from a gardener
   bypasses `defaultOrphanDeLaneCooldown` entirely, because the cooldown guard
   lives in the sweeper, not in the label.

**Why this is a rule and not a preference.** Cycle 2026-08-20 20:43 graded 7
rows `ORPHAN-READY` and "fixed" them by hand-applying `needs-intake`. All 7
already carried `project:gardenio` + a topical label, so they **passed RDY-3
before that cycle touched them** — RDY-3 is an **OR**, and reading it as an AND
is what produced the violation. On 4 of the 7 it was worse than a no-op: their
most recent lane action was the intake-groomer deliberately **de-laning** them,
so re-labeling took the sweeper's side in a documented fight-loop
(ka-0000007370, ka-0000008193, ka-0000007864) whose fix had already landed in
code. Three beads — ka-0000003955, ka-0000001471, ka-0000001597 — ping-ponged
between the two loops for **five weeks** before the operator had to break the
tie by hand.

**The generalizable failure, worth more than the specific rule.** Both loops
were individually correct and each re-derived its own policy from first
principles every cycle, so neither could see that the *contradiction between
them* was the defect. Two gardeners holding opposing policies about the same
label is a **coordination** bug, and it cannot be fixed inside either loop:
whoever acts last wins until the other one wakes up. When you find yourself
re-applying a mutation a sibling gardener has already reverted — check the bead's
comment history for the reversal before acting, and if you find one, **stop and
file the contradiction as a decision** instead of taking your side of it again.
The circuit breaker (three strikes → escalate) is cheap; five weeks of
ping-pong is not.

**A root-cause bug does not gate unsticking the rows.** The operator also
rejected `fix-the-root` *as the answer* while affirming the bug is real: the
`ListReady` cap (ka-0000000077) was a genuine truncation defect and hand-
labeling around it was a symptom. But "both loops hold off until the fix lands"
is precisely what cost the five weeks. Fix the root **and** unstick the rows;
never let the former block the latter. (That cap is now fixed —
`orphanStore.ListReady` pages exhaustively via `ListAllBeadsFiltered`.)

### 5.7 RDY-6 disposal — two passes, per-row, at the destructive cap

Operator ruling on decision [[ka-0000006214]] (Matt Baker, 2026-08-24,
`chosen=add-gate`): drain the `DEAD-PARENT` class **incrementally at the 10-row
destructive cap (§5.1), newest-dead-parent first**, every cycle. `accept-as-is`
was rejected in the operator's words — *"32 percent of the ready queue pointing
at finished epics is not an honest queue, it is a queue that has stopped meaning
anything"* — and a one-off triage was rejected because a gate keeps working after
the backlog drains.

**Pass 1 — auto-close the clearly-moot subset.** Predicate: **parent closed
>60d AND child untouched >60d**. Sound, but it is a *candidate filter, not a
verdict* — every row still gets the §5.0 two-leg decision gate plus a liveness
read before it closes.

**Pass 2 — work the mixed remainder per-row.** For each surviving row choose
explicitly between **close-as-moot** and **REPARENT to a live epic**. Never
leave it dangling.

**The predicate over-fires — inspect before closing.** On the first pass
(2026-08-24) it nominated 9 rows; **only 2 were actually moot.** What overrode
it, in each case evidence the timestamps could not carry:

- **A closed parent's own closing comment can name the child as remaining
  work.** ka-0000002176 closed 2026-06-05 with an explicit *"Remaining work:
  ka-0000002920, ka-0000000614"* list. Both matched the auto-close predicate on
  age; both are live work. **Read the parent's close comment before closing any
  child.**
- **A dead subject can be a teardown, not an abandonment.** The `gitlab-dark`
  workload is gone from the cluster (no ns/deploy/sts/svc/helm release), which
  reads as moot — but epic ka-0000004271 is OPEN and carries an operator
  DIRECTION to stand it back up on `prod-gardenio-eks`, and
  `deploy-values/gitlab-dark/` is still committed. Absent infrastructure is not
  absent intent.
- **`updated_at` is not "last touched"** (§4.3) — comments do not bump it, so a
  row with a substantive checkpoint comment can still look untouched for 80d.
  ka-0000007794 carried a completed leak analysis and is the hard prerequisite
  for a resolved gate (G9 → shared-RDS).

A close is only justified when the **subject itself** provably no longer exists,
verified independently of the bead. The two rows that qualified (ka-0000003102,
ka-0000001823, the `jdark_*` Dark-Jira data model) cleared three checks:
`kd count -l source:jira-dark` = **0**, zero non-test code references to
`jdark_*`, and the simulation superseded by the **real** Jira bridge that
shipped (23 `source:jira` beads, a full `jira_*.go` surface). Every close
comment must record that evidence plus its reversal command
(`kd update <id> --status=open`).

**Fix the generator too, or the class refills.** The upstream cause is the
close path, not the daemon cascade: `auto_close.go` skips already-closed parents,
so it cannot produce this shape. These parents were closed **manually** by crew
and gardeners declaring an epic done while children still dangled — measured at
**46 of 56** rows (bug [[ka-0000000504]]). The daemon-side fix (park orphaned
children to `blocked` on parent close) merged 2026-08-24 as `641e82b44`, and the
parent-tiebreak fix for §4.6's display defect merged as `573bae5be` — but
**neither is in the deployed build** (release `2026.236.1` predates both;
`kbeads:2026.235.1` was running when this gate was authored). Until they deploy,
RDY-6 is the only thing scoring this class, and the agent-side rule below is
what stops it refilling.

## 6. The gardener

The loop is implemented as a `type=trigger` / `role=gardener` bead pinned to
epic [[ka-0000004960]] via `gardener-epic:ka-0000004960`, modeled on the
meta-gardener (`gardener-meta-scoring-prompt.md`). Its playbook prompt is
carried on the trigger bead's `prompt` field; the canonical copy lives in
[`news-ready-relevance-gardener-prompt.md`](./news-ready-relevance-gardener-prompt.md).

Each wake the gardener runs:

1. **READ** — `gb news --project gardenio --all-projects` and
   `gb ready --project gardenio`, plus `gb news --stale-only --ids-only` for the
   zombie set and `kd list` cross-checks.
2. **SCORE** — apply §2 gates to every row; compute §4 relevance per queue;
   assign §3 grade. Apply §3 short-circuits first.
3. **FIX** — for each violation, apply the §5-authorized fix (logged) or file
   the escalation/decision. Batch-cap destructive actions at 10/cycle.
4. **REPORT** — write one `type=doc` scorecard bead per run under the live
   hygiene epic [[ka-0000000903]]: per-queue grade, relevance score, violation
   tallies by class, and the list of fixes applied (with bead ids) /
   escalations filed. Typed-mention each fixed bead so the scorecard is
   navigable in beads-viewer.

   > **The scorecard must be `type=doc`, never `type=task`.** `type=task` is
   > `kind=issue`, so every scorecard this loop wrote landed in `gb ready` as a
   > fake claimable row — measured 2026-08-20: scorecard ka-0000001397 was
   > **row 1** of `gb ready --project gardenio`, making this loop a *source* of
   > the noise it exists to remove. `type=doc` is `kind=data` and never reaches
   > ready. (Note ka-0000004960 — the original authoring epic named in v1 of
   > this doc — closed in 2026-08; new scorecards parent to ka-0000000903.)
5. **SQUAWK + EXIT** — one-line summary to the thread/channel:
   `ready: B (0.88, closed 6 cron-exhaust); news: C (9 zombies → 7 closed, 2
   escalated); scorecard kd-…`. If a decision is open → `gb yield`; else
   `gb stop`.

### Loop bounds (required, advice ka-0000000974)

> **This gardener MUST be marked STANDING** — set `"standing": true` in the
> gardener blob **and** attach the `perpetual:true` label. Queue hygiene is a
> *perpetual mandate with no terminal epic*: a clean cycle with nothing to fix
> legitimately scores **zero** progress, which is the healthy steady state, not
> a stall. Without a standing marker the engine treats a marker-less
> `no_progress_for` loop as an *epic-bounded* gardener and **reaps it after the
> resume budget is spent on clean cycles** — the exact failure that silently
> killed the original gardener `ka-0000005840` on 2026-06-23 (11 runs, 8
> consecutive clean cycles → self-close). A standing gardener instead **re-arms
> in place forever** on `no_progress_for` (`gardenerRearmStandingInPlace`,
> bug ka-0000006241), so the wedge detector below still provides D6 5/5 hygiene
> without the self-kill. See advice ka-0000004311 (standing-marker guard).

- **terminal_goal:** a STANDING mandate the standing-marker guard recognizes
  as perpetual (`ka-0000004311`), e.g. *"STANDING relevance-critic gardener
  (perpetual loop — does NOT self-complete on a momentarily-clean queue): keep
  `gb ready` + `gb news` for gardenio RELEVANT (relevance ≥ 0.9, 0 zombies > 1h,
  0 CRON-EXHAUST floods). A clean cycle with nothing to fix is SUCCESS, not
  stall."*
- **stop.loop_end:** `no_progress_for: 8` runs OR `max_runs` — kept as the
  progress-aware wedge detector (D6 5/5). Because the gardener is STANDING, a
  premature `no_progress_for` / `max_runs` stop re-arms it in place rather than
  closing it, so these bound a genuinely *broken* loop without ending a merely
  *quiet* one. Queue rot is *continuous*, so this is a long-running hygiene loop
  with a generous `max_runs` and a cron floor, not a one-shot drain.
- **hard_stop:** `consecutive_failures: 5` OR `wall_clock: 720h` — the only
  paths that close a standing gardener (a genuinely broken one). Revive an
  expired-`wall_clock` gardener in place by stamping `wall_clock_since=now` in
  the blob (or `gb trigger enable --rebaseline`) so it is not re-closed on the
  next reconcile (bug ka-0000006465).
- **progress events:** `child_closed` (a fix bead closed), plus a
  `wrapup_field progress_score >= 1` for cycles that closed zombies / cron
  exhaust even when no child bead changed.

## 7. Relationship to existing rubrics

- This rubric is to the **read-queues** what the
  [Gardener Effectiveness Rubric](./gardener-effectiveness-rubric.md) is to
  **gardeners**: the effectiveness rubric scores whether a gardener converts
  wakes into closed work; this rubric scores whether the *queues those agents
  read* are trustworthy. CRON-EXHAUST and ZOMBIE violations here are often the
  downstream symptom of a gardener scoring poorly on D3 (compute economy) /
  D4 (crew health) there — the two loops are complementary, and this gardener
  *reports* such symptoms to the meta-gardener rather than tuning the offending
  gardener itself.
- It does not fork the [data-correctness rubric](./data-correctness-rubric.md);
  that rubric governs *rendered metrics/traces*, this one governs *work-queue
  text*.
