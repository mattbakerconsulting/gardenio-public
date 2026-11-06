# needs-intake lane: drain architecture + the residual thread-bead gap

> **Tracking:** ka-0000001803 ("needs-intake lane fills but never drains — no
> adopt-into-epic step"). This doc is the forward map for that task. It records
> what the lane looks like **today** (2026-08-21), which drains exist, and the
> one class of bead that **still** has no drain — with the root cause and the fix.

## TL;DR

The bug as originally filed (2026-07-08: ~2100 beads labeled `needs-intake`,
almost none re-parented, **no adopt-into-epic step**) is **largely fixed**:

1. **Fire-record cruft (929 beads) is gone.** The stale-reaper gap
   (ka-0000000075, MR !5489) now reaps open never-claimed run-instance fire
   records, so they no longer masquerade as orphaned work in the lane.
2. **The adopt-into-epic step now exists.** The intake-groomer gardener
   (ka-0000003841, `enabled`, hourly `33 * * * *`) drains genuine work via a
   canonical **4-step de-lane whose step 1 is "PARENT it first if parentless"**
   (see [`runbooks/intake-groomer.md`](./runbooks/intake-groomer.md) §"THE ONE
   THING"). The controller `auto_intake_sweep.go` de-lanes parented plan/epic
   children in code. Adoption is no longer missing for *work* beads.

**But the lane's composition has inverted.** As of 2026-08-21 the open gardenio
`needs-intake` lane holds ~108 beads, of which:

| Slice | Count | Drained by |
|---|---:|---|
| Genuine work beads (task/bug/epic/feature, non-slack) | ~15 | intake-groomer (adopt→de-lane) ✅ |
| **Slack-thread beads (`source:slack` / `thread`)** | **~93** | **nothing — both drains exempt them** ❌ |
| — of those, **all bound agents already terminal (work done)** | **~78** | should auto-close; **doesn't** (see below) |

So the lane no longer "fills but never drains" for *work*. It fills-and-never-
drains for **finished Slack-thread beads** — a different leak, in a different
subsystem, with a concrete code root cause.

## Why the two intake drains do not touch Slack-thread beads (by design)

Both drains deliberately exempt Slack-sourced beads, because a Slack-thread bead
is a **human's wish**, owned by its thread agent, not generic orphaned work:

- **intake-groomer gardener** — [`runbooks/intake-groomer.md`](./runbooks/intake-groomer.md)
  §4 "Never touch": *"Beads labeled `thread`, `slack-thread`, or `source:slack`
  — they belong to human thread agents; leave them in the lane."*
- **controller auto_intake_sweep** —
  `gardenio/controller/internal/scheduler/auto_intake_sweep.go`,
  `isSlackSourced()` / `slackSourceLabelSubstrings` (`source:slack`,
  `slack-thread`, `slack-mention`): a bead whose intake gating came from a human
  Slack thread "stays human-owned; only labels applied by the generic automated
  sweep are promoted".

That exemption is **correct** — you do not want a janitor re-parenting a live
human conversation under a fleet-infra epic. The problem is not the exemption;
it is that the subsystem that *is* supposed to drain a finished thread bead —
the thread-task auto-closer — has a blind spot that leaves finished ones open.

## Root cause of the residual leak: the thread-task auto-closer's terminal-witness blind spot

The thread-task auto-closer
(`gardenio/controller/internal/bridge/thread_task_autoclose.go`) is what should
close a Slack-thread bead ~30 min after its bound agent(s) finish. It has two
paths:

- **Event path** (`MaybeSchedule`/`runCheck`): fires when an agent close event
  arrives. Robust while the bridge is up and the agent close is observed.
- **Reconcile sweep** (`sweepOnce`, runs on `DefaultThreadTaskSweepInterval`):
  the safety net for events lost to a bridge restart, a spawn-path linking gap,
  or beads created before the event path shipped.

The sweep reaps a thread bead **only on a positive `terminalWitness`** — proof
that a bound agent ran-and-finished (`thread_task_autoclose.go:~552`,
"positive-evidence-only" gate; the change that intentionally removed the older
negative-evidence `!liveByLabel` reap, 1b-8 / ka-0000000126). That witness is
built from `ListAgentBeads`, which is **active-status-only**:

```go
// beadsapi/client.go
var activeStatuses = []string{"open", "in_progress", "blocked", "deferred"}
func (c *Client) ListAgentBeads(ctx) ... { return c.listBeads(ctx, []string{"agent"}, activeStatuses) }
```

The code even flags the hazard in `buildMemberEvidence`:

> `// A member in a terminal agent_state is the clean ran-and-finished witness`
> `// (1b-8). ListAgentBeads is active-status only, but an active-status bead can`
> `// still hold a terminal agent_state awaiting reap.`

So the terminal witness is only visible during the **brief window** between an
agent reaching `agent_state ∈ {done,failed}` and the **agent bead itself
closing** (dropping to `status=closed`, out of the active-status listing). If
the sweep does not run inside that window — bridge restart, sweep interval
lands after the agent bead closes, event lost — the witness is **gone forever**.
From then on the thread bead has:

- no live member (all agents terminal → not `ev.live`),
- **no terminal witness** (all agent beads closed → out of `ListAgentBeads`),

so the positive-evidence gate takes the *"leave it OPEN"* branch **every sweep,
forever**. The thread bead is stranded open, keeps its `needs-intake` label
(the orphan sweep re-applies it since it is parentless and childless-of-open),
and **neither intake drain will touch it** (Slack-exempt). Dead end.

**Measured (2026-08-21):** 78 of the 93 Slack-thread `needs-intake` beads have
**all bound agents in a terminal state** yet remain open — many dating back to
**2026-06-25**. None carry `needs-triage` (so they are not the failed-agent
operator-owned class); they are simply finished wishes the sweep can no longer
prove were worked, because the proof aged out.

### Verify it yourself

```sh
# The stranded set: open gardenio slack-thread beads whose bound agents are all closed.
kd list --all-projects -l needs-intake -l thread --status=open --all-types \
  --json --limit 5000 \
  | jq -r '.[] | select((.labels|join(","))|test("project:gardenio")) | .id'
# For any id: kd dep children <id> shows all type=agent children status=closed,
#   agent_state=done — yet the thread bead is still open.
```

## The fix (design)

The exemption stays; the auto-closer's evidence gate must survive the agent
bead closing. Two viable shapes (a follow-up bug carries the implementation):

**Option A — durable terminal witness (preferred).** When an agent reaches a
terminal state, stamp a durable marker the sweep can read without needing the
agent bead to still be active — e.g. a `thread_agent_terminal_at` field (or a
closure-comment marker) on the **thread bead itself**, written by `runCheck` at
close time. The sweep then treats "thread bead carries a terminal-witness
marker AND is idle past the window AND has no live member" as authorization to
close. This preserves the positive-evidence principle (we still require proof a
member ran) while making the proof durable instead of racing the agent-bead
reap. Symmetric with how the failed→`needs-triage` split already stamps a
durable label that survives the failed agent bead closing.

**Option B — widen the witness listing.** Give the sweep a second, closed-status
agent-bead lookup **scoped to the thread label** (not a full closed-agent scan —
that is the OOM/truncation hazard of advice ka-0000003555), so a recently-closed
terminal agent still counts as a witness. Bound the lookback (e.g. closed within
N days) to keep it cheap. Weaker than A: still time-bounded, and pays a
per-thread round-trip.

Either way the acceptance test is: **a thread bead whose only agent ran, went
`done`, and then had its agent bead closed is auto-closed by the sweep** — the
exact case the active-status-only witness misses today.

## What this doc changes / does not change

- **No code lands here.** Per advice ka-0000001560 this investigation is a doc
  bead + a follow-up bug, not an MR. The code fix ships under the follow-up bug
  with its own MR + release (controller image).
- **The adopt-into-epic step is documented as already-present** for work beads
  (intake-groomer 4-step de-lane, step 1). No new gardener is needed for that
  half; standing up a *third* triage gardener to re-parent work would fight the
  groomer (janitor↔groomer fight-loop, decisions ka-0000008119 / ka-0000002231).
- **The residual gap is a bridge auto-closer bug, not an intake-lane bug.** The
  correct fix is in `thread_task_autoclose.go`, not a new adopt-into-epic
  gardener — a gardener cannot re-parent Slack-thread beads without violating the
  human-owned exemption both drains rely on.

## Provenance

- Investigation: task ka-0000001803 (2026-08-21), crew agent.
- Fixed-since-filing inputs: reaper-gap ka-0000000075 (MR !5489); root-cause
  ka-0000000273; groomer revival ka-0000003841; audit ka-0000005229
  (/bundle/ka-0000001631).
- Companion runbooks: [`runbooks/intake-groomer.md`](./runbooks/intake-groomer.md)
  (sweep-OUT), [`runbooks/triage-intake-janitor.md`](./runbooks/triage-intake-janitor.md)
  (sweep-IN).
- Code refs: `internal/scheduler/orphan_sweep.go` (`isOrphan`),
  `internal/scheduler/auto_intake_sweep.go` (`isSlackSourced`),
  `internal/bridge/thread_task_autoclose.go` (`sweepOnce`,
  `buildMemberEvidence`), `internal/beadsapi/client.go` (`activeStatuses`,
  `ListAgentBeads`).
