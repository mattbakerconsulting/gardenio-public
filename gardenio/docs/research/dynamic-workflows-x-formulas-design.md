# Dynamic Workflows × Formulas: Interaction & Integration Design

*Design doc — bead [ka-0000007305]. Parent research epic [ka-0000005692]. Follows the
completed dynamic-workflows pilot [ka-0000006835] (public-sanitize sweep). Deferred
per operator steer (Matt, 2026-05-28): "interaction with formulas are more
interesting, but can be deferred until after pilot."*

> **Status: research / direction.** This doc was produced by a dynamic-workflow
> design run (5 parallel dimension agents → synthesis → adversarial review) and
> then reconciled against the actual `kd`/scheduler surface. Mechanisms are tagged
> **[exists]** (verified in the current codebase), **[net-new]** (requires engine/CLI
> work — captured as a follow-up bead), or **[proposed convention]** (a usage pattern
> on top of existing primitives). Do not treat the **[net-new]** shell snippets as
> runnable today.

## Executive Summary

Gardenio has two distinct fan-out primitives, and conflating them wastes pods and tokens.

- **Formulas** materialize bead-tracked **pod fan-out**: an `apply_formula` trigger (or
  `expand` / `on_complete` `for_each`) creates N step beads, each claimed by its **own**
  pod with its own Claude session, git checkout, and per-bead `total_cost_usd`. The right
  shape when work must **diverge** — separately reviewable MRs, independently mergeable
  artifacts, per-item human gates, isolated failure, durable per-item audit/cost.
- **Dynamic workflows** are **single-pod, in-session fan-out**: one `spawn_agent` trigger
  creates one task bead and one pod, and that pod runs a JS orchestration script that fans
  out via `agent()` / `parallel()` / `pipeline()` as in-session subagents sharing one token
  pool, one `coop --resume` state, one checkout. The right shape for **embarrassingly-parallel
  sweeps** — short, read-only or single-MR work converging on one decision.

**They compose.** A formula *step* can host a dynamic-workflow run: the step bead is the
durable `in_progress` anchor; the host pod runs the workflow in-session, writes a structured
result back onto the step bead via `kd update --field`, and a downstream step (or `on_complete`)
consumes that result. This pays the cheap-*overhead* in-session cost for the fan-out (one pod
cold-start, one credential context, one bead) and the expensive per-pod cost only for items
that earn a durable bead/MR.

The design hinges on four pieces, each of which is partly net-new engine work (see *Proposed
follow-up beads*):

1. A **result-write contract** — the host pod writes `dwf_result` (succeeded/failed arrays)
   onto the step bead with `kd update --field` **[exists]**, but threading those arrays into a
   downstream `on_complete` expansion needs `{{step.output.X}}` resolution **[net-new]**.
2. A **partial-completion convention** so the success path is never gated on stragglers
   (overloads existing `closed` + a label today; a first-class `partial` status is **[net-new]**).
3. A **cost rollup** that **sums** the host pod's `session_of` usage **plus** a host-reported
   `workflow_spend` field (the two measure *non-overlapping* spend — see §3) to close the
   **KEY GAP** that in-session subagents get no beads.
4. A **runId-persistence + S3-journal** protocol that makes a workflow-hosting step resumable
   across pod restarts — necessary because dynamic-workflow resume is **same-session only**.

## Mental model: two fan-out primitives

| | Formula fan-out (pod fan-out) | Dynamic workflow (in-session fan-out) |
|---|---|---|
| **Trigger** | `apply_formula` / `expand` / `on_complete for_each` | `spawn_agent` (1 task bead, 1 pod) |
| **Unit of work** | N step beads, each claimed by its own pod | N in-session `agent()` subagents in one pod |
| **Sessions / checkouts** | N Claude sessions, N coop states, N checkouts | 1 session, 1 coop state, 1 checkout |
| **Per-item bead** | Yes — real bead w/ status, owner, cost | No — subagents invisible to beads (**KEY GAP**) |
| **Cost telemetry** | Per-bead `total_cost_usd` | Aggregate only (host pod must self-report) |
| **Git artifact** | Per-item branch/MR/worktree | Shared checkout; one MR or read-only |
| **Per-item worktree isolation** | Inherent (each pod is its own checkout) | Optional via the Workflow tool's `isolation:'worktree'` (expensive; rarely needed) |
| **Failure isolation** | One bead fails, others unaffected; reclaimable | One pod death loses the run (resume is same-session) |
| **Human gating** | Per-item `gate` beads | One `gate` over the whole batch |
| **Scale bound** | gardener `run_budget`/`max_runs`, pod capacity | `min(16, cores-2)` concurrent, ≤1000 lifetime subagents |
| **Best for** | Divergent, durable, independently-mergeable work | Short, coupled, embarrassingly-parallel sweeps |

Choose by whether the **outputs need to diverge**.

---

## 1. Decision framework: dynamic-workflow run vs. formula-driven pod fan-out

When a formula step decomposes into N work items, you choose between two execution shapes:

- **(A) Dynamic-workflow run** — one `spawn_agent` trigger creates one task bead and one pod;
  that pod runs the JS orchestration script and fans out via `agent()`/`parallel()`/`pipeline()`
  as in-session subagents. One token pool, one `coop --resume` state, one git checkout.
- **(B) Formula sub-graph** — an `apply_formula` trigger (or `expand`/`on_complete for_each`)
  materializes N step beads. Each becomes ready, gets claimed, and spawns its **own** pod
  (separate Claude session, separate coop state, its own per-bead session usage and
  `total_cost_usd`).

### Deciding axes

| Axis | Favors (A) dynamic-workflow | Favors (B) formula fan-out |
|---|---|---|
| **Per-item audit trail** | Aggregate is fine; subagents are invisible to beads (the KEY GAP) | Need per-item tokens / `total_cost_usd` / status / owner on a real bead |
| **Per-item git artifact** | Shared checkout; one branch/MR, or read-only audit | Each item needs its own branch → MR; each pod is its own checkout |
| **Work-item independence** | Tightly coupled to a single merge/decision gate | Independent, separately mergeable/deployable |
| **Per-item duration & cost** | Short (seconds–low minutes), cheap, many items | Long-running, expensive, few items — pod overhead amortizes |
| **Human-gating granularity** | One `gate` (decision/manual) over the whole batch | Per-item `gate` beads, independent approve/reject |
| **Failure isolation** | One pod death loses the run; resume is runId-based, **same-session only** | One pod/bead fails in_progress; others unaffected; failed bead reclaimable independently |
| **Scale** | `min(16, cores-2)` concurrent, ≤1000 lifetime subagents, one pod's resources | Bounded by gardener `run_budget`/`max_runs` and cluster pod capacity |

### Worked gardenio examples

1. **Public-sanitize sweep ([ka-0000006835])** → **(A)**. Embarrassingly-parallel read-only
   audit + adversarial verify, converging on a *single* merge-gate decision. No per-file MR,
   no per-file bead needed. One pod, `parallel()` for audit, `agent()` for verify, one `gate`.
   The canonical (A) — this is what the pilot actually did.

2. **Multi-repo release** → **(B)**. Each component (`gardenio/controller`, `kbeads`, `coop`,
   `beads3d`) gets its own build/tag/deploy artifact, its own commit status, and must fail
   independently without blocking siblings. Use `apply_formula` with `children` or `on_complete
   for_each`, one step bead per component, each pod owning its checkout. Per-bead `total_cost_usd`
   rolls the release cost up by component.

3. **Docs port (N pages → N MRs)** → **(B)**. Each page is independently reviewable and
   mergeable; reviewers want one MR per page and a bead to track status. `expand` a per-page
   formula; each pod creates its own branch/MR. If instead it were one mechanical rewrite
   landing in a single MR, flip to **(A)**.

4. **E2E-flake triage** → **(A)** with a **(B)** escalation tail. Fanning out across failing
   modules to *classify* flake-vs-real is short, read-only log-reading converging on one triage
   report — dynamic-workflow with `pipeline()` per module. Then thread the confirmed-real
   failures into a (B) sub-graph: one fix bead + pod + MR per real bug (see §2 for the threading
   mechanics and its caveats).

### Default and exceptions

**Default: (A) dynamic-workflow.** Most fan-outs are short, read-only or single-MR analysis
converging on one decision; the in-session model has lower *overhead* (no per-pod cold start,
one coop session, one bead) and is simpler to reason about. (Note: in-session subagents still
consume the shared token pool — the saving is overhead, not compute.)

**Switch to (B)** when *any* of these is true: each item needs its own MR/worktree; each item
needs an independent bead for audit/cost/human-gating; items must fail in isolation; or per-item
work is long/expensive enough that pod overhead is negligible.

**Hybrid (preferred for triage-then-fix):** run (A) to scan/classify, then thread results into
a (B) sub-graph only for items that earned a pod (§2). Cheap fan-out stays cheap; pod cost is
paid only where a durable bead/MR is justified.

**Resume caveat (a design constraint, not a mode toggle):** an (A) run inside a formula step is
fragile across pod restarts — the step bead stays `in_progress` but the runId is lost unless
persisted (see §4). If a step is long enough that mid-run pod death is likely, either persist
the runId (§4) or decompose into (B) where each bead is independently reclaimable.

---

## 2. Threading dynamic-workflow output through a formula step

### 2.1 The result-write contract

A dynamic-workflow-hosting step bead carries `source_formula`/`source_step_id` like any step
**[exists]**. When the host pod finishes its in-session workflow it writes a *structured* result
back onto its own step bead before resolving it. We write it to a typed field `dwf_result`
(JSON-encoded string) using the existing repeatable `--field` flag — **no new CLI surface
needed for the write itself**:

```json
{
  "schema": "dwf.result.v1",
  "succeeded": ["controller", "kbeads", "coop"],
  "failed": [
    {"item": "beads3d",      "reason": "vite build OOM",       "retryable": true},
    {"item": "slack-bridge", "reason": "lint: unused import",  "retryable": true}
  ],
  "total": 5,
  "run_id": "wf_bbba80e9"
}
```

```sh
# [exists] kd update --field key=value is real (repeatable -f/--field).
kd update ka-STEP --field dwf_result="$(cat /tmp/dwf-result.json)"
```

`run_id` is load-bearing for re-entrancy (§4): persist it so a restarted pod resumes the
workflow instead of restarting.

### 2.2 Consuming the result downstream

There are two consumption paths. **Be precise about what exists**, because the synthesis draft
conflated them:

**(a) `kd formula expand` over a success array — [exists] for the array, [net-new] for output
auto-resolution.** `kd formula expand <step-bead> --values a,b,c [--var k=v]` is real
(`kbeads/cmd/kd/formula_expand.go`): it reads the step bead's `on_complete` spec, iterates the
`for_each` var over `--values`, and substitutes `expand_vars` **against the loop var only**. So
this works today **if a human/host pod supplies the values explicitly**:

```sh
# [exists] — values supplied explicitly by the host pod after reading dwf_result.
kd formula expand ka-SCAN-STEP --values controller,kbeads,coop
```

What does **not** exist: automatic resolution of `{{scan.output.succeeded}}` *from the bead's
`dwf_result` field* into `--values`, and threading a *scalar* like `{{scan.output.run_id}}` onto
every expanded molecule. `--values` is a positional list, not a key/value map; `expand_vars`
templating only sees the loop var. Closing this — a `kd formula expand --from-field dwf_result.succeeded`
(or having the engine resolve `{{step.output.X}}`) — is **[net-new]** (follow-up bead #2). Until
then, the host pod is the glue: it reads its own `dwf_result`, then calls `kd formula expand
--values <succeeded,joined,by,commas>` itself.

**(b) A plain downstream step that reads `dwf_result`.** The simplest path needs no new
templating at all: the next formula step's prompt instructs its pod to read the upstream step's
`dwf_result` field (`kd show <upstream-step> --json | jq .fields.dwf_result`) and act on it. This
is fully **[exists]** today and is the recommended near-term pattern; `on_complete` auto-threading
is the ergonomic upgrade.

### 2.3 Partial-failure semantics

The hard question: with 8/10 succeeded, how does the step resolve so the 8 proceed without the
2 stragglers blocking forever?

**Near-term [exists]:** the host pod closes the step normally (`kd close ka-STEP`) **after** it
has (i) written `dwf_result`, and (ii) **created the follow-up failure beads first** (see below).
A `had_failures` label flags the partial outcome for humans and gardeners. Closing is safe *because*
the failures are already durable, blocking beads — closing the step does not lose them.

**Net-new upgrade:** a first-class `partial` status (valid statuses today are
`open|in_progress|blocked|deferred|closed`, per `kbeads/cmd/kd/status.go` — `partial` does **not**
exist) that the engine treats as terminal-for-progression and that fires `on_complete`. This is
cleaner than overloading `closed`+label but requires a state-machine change (follow-up bead #1).

**Ordering matters (corrected from the draft):** create the follow-up failure beads **before**
resolving the step, never after. If the step is closed/`partial` first and `on_complete` fires,
a downstream `waits_for: all-children` join could evaluate against the parent epic *before* the
follow-up beads exist and falsely satisfy itself. Correct sequence:

```sh
PARENT=$(kd show ka-STEP --json | jq -r '.parent // .dependencies[]?|select(.type=="parent-child").id' | head -1)
# 1. Durable failures FIRST — each blocks the parent epic.
for f in beads3d slack-bridge; do
  kd create "dwf follow-up: $f failed under $(kd show ka-STEP --json | jq -r .title)" \
    --type task -l project:gardenio -l dwf-followup -l retryable \
    -d "Auto-filed from dynamic-workflow run on step ka-STEP. source_step_id=ka-STEP"
  # then: kd dep add <new> $PARENT --type parent-child ; kd dep add $PARENT <new> --type blocks
done
# 2. THEN write result + resolve the step.
kd update ka-STEP --field dwf_result="$(cat /tmp/dwf-result.json)" -l had_failures
kd close ka-STEP
```

This keeps three concerns separate: **threading** (`dwf_result.succeeded` → downstream work),
**progression** (the step resolves so the happy path proceeds), and **failure durability**
(confirmed failures are blocking follow-up beads carrying `source_step_id`, not lost array
entries). The `retryable` label drives whether a gardener re-dispatches them.

---

## 3. Molecule-level cost telemetry (post-spend)

Pre-spend guardrails are explicitly out of scope (closed [ka-0000003518]); post-spend reporting
is in.

### 3.1 Roll-up: from session beads to a molecule total — [net-new walker, exists data]

Per-session usage already attaches to agent/task beads via `beadsapi` `client_session.go`
(`input_tokens`, `output_tokens`, `thinking_tokens`, `cache_read_tokens`, `cache_write_tokens`,
`total_cost_usd`) **[exists]**. A molecule's step beads hang off it via `parent-child`; each
claimed step's pod and any spawned sub-agents carry session usage on their beads. The rollup is
a BFS over `parent-child` reverse-deps, summing the existing usage fields:

```text
molecule
  └─ step bead            (parent-child)
       ├─ session usage    (on the agent/task bead)
       └─ spawned agent    (parent-child) → its own session usage
```

Implement as `beadsapi.Client.RollupMoleculeUsage(ctx, molID)` — a **read-side** walker over
existing data. **[net-new]** (follow-up bead #5).

### 3.2 The KEY GAP: dynamic-workflow internal subagents — sum, don't replace

A step that *is* a dynamic-workflow run spawns one pod; its in-session `agent()` subagents never
get beads, so the rollup above sees only the host pod's own session. **Crucially (corrected from
the draft): the host pod's `session_of` usage and the subagent spend are *non-overlapping*.** The
host session measures the orchestrator/driver tokens; the subagents are a separate pool invisible
to beads. So the correct subtree total is **host-session usage + reported subagent spend**, a
*sum*, not a "prefer one over the other."

The host pod reports the subagent pool via a new `workflow_spend` field on its step bead, written
with `kd update --field` **[exists write path]** from the orchestration script's completion handler
(and at each resume checkpoint, `final:false`, so a mid-run pod death doesn't lose it):

```json
"workflow_spend": {
  "agents_spawned": 47,            // cumulative over the run, NOT concurrent (cap is min(16,cores-2))
  "input_tokens": 3120000, "output_tokens": 410000,
  "cache_read_tokens": 8800000, "total_cost_usd": 62.40,
  "run_id": "wf_bbba80e9", "reported_at": "2026-05-29T18:40:00Z", "final": true
}
```

On resume the field is overwritten — idempotent on `run_id`. The rollup adds
`workflow_spend.total_cost_usd` **to** the host step's session usage for that subtree. The
**[net-new]** part is the rollup-side awareness of `workflow_spend` and the script-side reporter
(follow-up bead #6).

### 3.3 Operator surface — [net-new]

A new `gb molecule cost <molecule-id>`:

```
$ gb molecule cost ka-Abc123
molecule ka-Abc123  (formula ka-0000000359)
  steps: 6   sessions: 11   workflow-runs: 1 (47 internal agents, cumulative)
  cost:  $84.10  (host sessions $21.70 + workflow_spend $62.40)
    input 4.1M  output 0.6M  thinking 0.2M  cache-read 12.3M  cache-write 0.4M
  by step:
    scan          ka-s1   $3.20    1 session
    fan-out (wf)  ka-s2  $63.20    host-session $0.80 + workflow_spend $62.40 (47 agents)
    merge-gate    ka-s3   $1.90    1 session
```

Note the workflow step's cost is the **sum** of its host-driver session and the reported
subagent spend — no "unattributed overhead" line, because nothing is discarded. `--json` emits
the rolled-up usage plus per-step breakdown. A beads-viewer `/molecule/{id}/cost` card can reuse
existing utilization-bar rendering.

### 3.4 Minimal schema additions

- **Step bead**, new field `workflow_spend` (object above) — populated only for dynamic-workflow
  steps. Reuses the existing usage shape plus `agents_spawned`, `run_id`, `reported_at`, `final`.
- **(optional) Molecule bead**, cached `usage_rollup`. **Caveat (corrected):** a cache needs an
  *invalidation hook* on session close — that is a **new write path**, contradicting "read-only."
  Recommendation: ship the read-side walker first (no cache, always-correct), and only add a cache
  (with an explicit `CloseSession` ancestor-invalidation hook) if the walk proves too slow. Also
  surface a `partial: true` flag whenever any descendant `workflow_spend.final` is false, so a
  mid-run `gb molecule cost` is not silently undercounted.

No new bead types, no new edge kinds — `parent-child` already carries the graph.

---

## 4. Re-entrancy: resuming a workflow-hosting step after interruption

### 4.1 Why this is hard

Dynamic-workflow resume is `Workflow({scriptPath, resumeFromRunId})` and is **same-session only**
— the runtime replays the unchanged prefix of `agent()` calls from a **journal** on local disk.
A pod restart (OOM/eviction/redeploy) loses both the local journal and the in-memory runId. The
step bead, however, stays `in_progress` across the restart. So resume requires persisting *both*
the runId and the journal off-pod.

### 4.2 Persisted state: three new step-bead fields — [net-new fields, exists write path]

Written with `kd update --field` **[exists]**:

- `workflow_script_path` — repo-relative path, e.g.
  `gardenio/controller/workflows/sanitize-sweep.js`. Written at pour/expand time from the formula
  step spec, so it survives a crash before the run even starts.
- `workflow_run_id` — the runId from the first `Workflow({scriptPath})` call. Written by the host
  pod **immediately after the run is created, before the first `agent()` settles**.
- `workflow_s3_uri` — the `s3://…/<runId>/` prefix where the journal is persisted (via coop's
  `upload_workflow_dir`/`restore_workflow_dir`, the same session-S3 lifecycle as transcripts; see
  `docs/research/s3-resume-e2e.md`).

The only un-resumable window is a crash between create-run and persist-runId; it costs at most
the first journaled prefix. (Open question: pre-allocating the runId before the pod starts would
eliminate even that — see Open Questions.)

### 4.3 Startup resume protocol — [net-new entrypoint logic]

After the existing `coop --resume` + `gb prime`, the entrypoint runs a rehydration check.
**This is proposed logic, not a current entrypoint** — and it uses `kd show --json | jq` rather
than the invented `kd get --field` from the draft. Note the journal is **already on local disk**
by this point: `coop --resume` restores the workflow companion dir before this step (see §4.4),
so there is no separate `s3-resume pull` — the resolved design spike (ka-0000002347) dropped that
phantom binary:

```sh
# proposed entrypoint addition, post coop --resume + gb prime
STEP=$(kd list --status in_progress --field "workflow_script_path=*" --assignee "$AGENT_ID" --json \
        | jq -r '.[0].id // empty')   # NOTE: a field-presence filter is itself [net-new]; until then,
                                      #       the prompt tells the pod its own step id directly.
if [ -n "$STEP" ]; then
  RUN_ID=$(kd show "$STEP" --json | jq -r '.fields.workflow_run_id // empty')
  SCRIPT=$(kd show "$STEP" --json | jq -r '.fields.workflow_script_path // empty')
  if [ -n "$RUN_ID" ]; then
    # journal already restored by coop --resume (§4.4); just invoke:
    # Workflow({scriptPath: SCRIPT, resumeFromRunId: RUN_ID})
    :
  fi
  # else: cold restart — workflow_run_id was never persisted (the un-resumable window).
fi
```

`gb workflow rehydrate <step>` wraps the lease gate + resume-plan emission for this flow; its
default journal-restore is a no-op precisely because coop already did the pull.

### 4.4 Interplay with `coop --resume`

`coop --resume` restores the **Claude conversation transcript** — the tool-call/result history
of the host session. It **also** restores the Workflow journal — the runtime's own on-disk replay
log of settled `agent()` calls — from the same session's S3 prefix. `restore_workflow_dir`
(`coop/crates/cli/src/s3.rs`) repopulates the Claude companion dir
(`<config>/projects/<project>/<uuid>/` — the per-run `agent-*.jsonl` + run-state) from the
`workflow/` prefix, and it is invoked on the resume path (`run.rs`) right after transcript
restore. So a single `coop --resume` brings back BOTH the conversation and the journal.

> **Design update (ka-0000002347):** an earlier draft of this section said `coop --resume` does
> **not** restore the journal, and prescribed a separate `s3-resume pull` before resume. Coop's
> `restore_workflow_dir` wiring post-dates that draft and closes the gap — there is no separate
> pull and no `s3-resume` binary (it was never built). `gb workflow rehydrate`'s journal-restore
> hook is now a documented no-op.

- **Same pod, in-session retry** (rare): journal still on local disk → native replay, no S3.
- **Pod restart / eviction** (the real case): local disk is gone; `coop --resume` brings back the
  conversation **and** the journal (via `restore_workflow_dir`) in one pass. Resume works as long
  as the journal was persisted (via `upload_workflow_dir` in the S3 subscriber's shutdown drain).
  Without a persisted journal, even a correct runId yields a cold start (no prefix to replay).

### 4.5 Idempotency and the double-dispatch risk

- **Prefix replay, not re-execution.** `resumeFromRunId` returns cached outputs of settled
  `agent()` calls without re-spawning them; re-execution begins at the first non-journaled call.
  The completed prefix costs zero tokens.
- **Bead-write idempotency at the seam.** The one re-run call straddling the crash may have
  already committed a bead side-effect. Since there is **no** `kd create --idempotency-key`
  today (**[net-new]** if we want it), the near-term pattern is **check-then-write**: before
  creating, `kd list -l dwf-followup --field "source_run_id=<run_id>" --json` (again, field-value
  filtering is the [net-new] dependency) or, until that exists, query by label + dedup in the
  host script. Worst case is a duplicate follow-up bead, not a corrupted graph.
- **Double-dispatch risk (corrected — the draft's "reaper won't free an in_progress step" claim
  is unverified).** Nothing in the verified codebase guarantees a gardener or stale-claim reaper
  won't re-dispatch a long-stale `in_progress` step (advice [ka-0000007726] explicitly says
  gardeners may reclaim in_progress beads >1h with no live agent). Two pods sharing one
  `workflow_run_id` would both restore the journal (via `coop --resume`) + replay — bead writes
  are guarded by check-then-write, but the *token spend* of two concurrent replays is not. **This needs a real
  lease**, not an assumption: e.g. a `workflow_lease` field (pod id + heartbeat ts) that the
  resuming pod checks and refreshes, and that stale-claim logic respects. Captured as a risk on
  follow-up bead #9.

---

## 5. Gardener heuristic for recommending dynamic workflows per step shape

### 5.1 The decision the gardener makes

When a gardener (a `type=trigger` bead with `triggers:`/`progress:` fields) finds a `ready` step
(deps satisfied, unclaimed), it normally spawns one pod via `spawn_agent`. The new heuristic
inserts a classification pass before dispatch: **single pod running a dynamic workflow, or plain
pod fan-out?**

Signals, all readable off the bead without spawning anything:

1. **Explicit annotation** — `exec_hint` field on the step (below). If present, it wins.
2. **`requires_skills` uniformity** — if every projected sub-item needs the *same* skill set,
   in-session fan-out is safe (one image, one credential context). Heterogeneous → separate pods.
3. **Inbound fan-out width (corrected timing)** — count the items threaded *into this step's*
   `expand_vars` / `--values` (i.e. the array produced by an upstream step). Do **not** read
   "this step's output" — a ready step has not run yet, so its own output is empty. ≥3 independent
   items with no inter-item deps is the sweet spot for `pipeline()`.
4. **Keyword scan** — title/description matches `audit|sweep|sanitize|classify|per-file|per-component|embarrassingly parallel|fan-out`. (This is how the pilot [ka-0000006835] would self-identify.)

Two-or-more positive → recommend dynamic workflow.

### 5.2 Step-level annotation — [net-new field, exists mechanism]

```yaml
steps:
  - id: sanitize-sweep
    title: "Audit + adversarial-verify each component"
    requires_skills: [code-review]
    exec_hint: dynamic-workflow      # dynamic-workflow | pod-fanout | separate-mrs
```

`exec_hint: dynamic-workflow` tells the gardener to spawn a single pod whose task prompt instructs
it to run a `parallel()` audit + adversarial-verify + single merge-gate (the pilot shape), and to
persist the runId per §4. Absent the field, the gardener falls back to the four-signal score.
`exec_hint` is a plain typed field, so it requires no schema change beyond the gardener reading it
(follow-up bead #10).

### 5.3 Prompt language for crew/gardener nudges

> Before spawning, classify this step. If it expands into ≥3 independent sub-items that share one
> skill set and produce no separate MRs (audits, classifications, in-place rewrites), run them as
> ONE pod via a dynamic workflow: `parallel()` the sub-items, then a single merge-gate `agent()`.
> One session, one checkout, one bead. Do NOT fan out N step beads for this — that burns N pod
> cold-starts and N credential refreshes for work one pod resolves. Conversely, if any sub-item
> needs its own MR, its own worktree, individual cost telemetry, or a distinct `requires_skills`,
> fan out separate step beads / pods so each gets its own bead, branch, and cost row.

### 5.4 Guardrails (advice-bead text)

```
ADVICE: dynamic-workflow vs pod-fanout per step shape

RECOMMEND dynamic-workflow when ALL hold:
  - exec_hint:dynamic-workflow OR >=2 of {uniform requires_skills,
    >=3 independent inbound fan-out items, audit/sweep/classify keyword}
  - work is in-place (no new branch/MR per sub-item)
  - sub-items share one skill set + one credential context

DO NOT recommend dynamic-workflow when ANY hold:
  - sub-item produces its own MR/PR or needs its own worktree
  - sub-items need individual cost telemetry / their own task bead
    (KNOWN GAP: in-session subagents get no bead; their spend is only
     visible if the host pod reports workflow_spend; see cost design)
  - sub-items have inter-item deps (use needs/depends_on step beads)
  - human gate/approval needed per item (use gate steps, not agent())

REMEMBER: pod-fanout = N beads, N branches, N cost rows, N pods.
dynamic-workflow = 1 bead, 1 checkout, 1 cost row, <=min(16,cores-2)
concurrent subagents. Choose by whether the OUTPUTS need to diverge.
```

File as a `type=advice` bead linked from the gardener formula via `extends`, so every crew member
claiming a step under that gardener reads it during `gb prime`.

---

## Proposed follow-up beads

Tagged with the verified-vs-net-new reality so each is independently shippable.

1. **(feature)** First-class `partial` step status: add to the status enum
   (`kbeads/cmd/kd/status.go`) and make the engine treat it as terminal-for-progression that fires
   `on_complete`. *Alternative if rejected:* keep `closed` + `had_failures` label (works today). — §2.3
2. **(feature)** `on_complete` output threading: resolve `{{step.output.X}}` (or add
   `kd formula expand --from-field dwf_result.succeeded`) so a closed step's structured result
   auto-feeds the downstream expansion's `--values`/`expand_vars`. Today the host pod is manual glue. — §2.2
3. **(feature)** Host-pod result-write helper: an orchestration-script completion handler that
   emits `dwf_result` (v1 schema) and resolves the step via the corrected ordering
   (follow-ups first, then resolve). — §2.1, §2.3
4. **(task)** Gardener/crew convention + helper for materializing `failed[]` as blocking follow-up
   beads carrying `source_step_id` + `retryable`, wired into a `waits_for: all-children` join. — §2.3
5. **(feature)** `beadsapi.Client.RollupMoleculeUsage` — read-side `parent-child` BFS summing
   existing session usage. No cache in v1. — §3.1
6. **(feature)** `workflow_spend` step-bead field + orchestration-script reporter (final +
   per-checkpoint, idempotent on `run_id`); rollup **sums** it with the host session. — §3.2
7. **(feature)** `gb molecule cost <id>` CLI (text + `--json`) + beads-viewer `/molecule/{id}/cost`
   card; show host-session + workflow_spend split and a `partial` flag when any `final:false`. — §3.3, §3.4
8. **(feature)** `workflow_script_path` / `workflow_run_id` / `workflow_s3_uri` step-bead fields +
   runId-first persistence ordering. — §4.2
9. **(feature)** Entrypoint workflow-rehydration check (journal already restored by
   `coop --resume`; no separate binary — ka-0000002347), **plus a `workflow_lease` (pod-id +
   heartbeat) to prevent double-dispatch** that stale-claim/gardener reclaim respects. — §4.3, §4.5
10. **(feature)** Gardener dispatch-time classifier: `exec_hint` field + four-signal score (with the
    corrected inbound-fan-out timing), and the `type=advice` guardrail bead linked via `extends`. — §5
11. **(task)** Supporting CLI primitives several items above assume: field-presence /
    field-value filtering on `kd list` (`--field key=*` / `key=val`) and optionally
    `kd create --idempotency-key`. Scope and decide which are worth building vs. working around. — §2.2, §4.3, §4.5

## Open questions for operator

1. **`partial` status** — add a first-class terminal-for-progression status, or stay with
   `closed` + `had_failures` label to avoid touching the state machine? (§2.3)
2. **Cost model confirmation** — confirm host-session usage and `workflow_spend` are
   non-overlapping (driver vs. subagents) so the rollup **sums** them. If they overlap on any
   platform, we need a different attribution rule. (§3.2)
3. **S3-journal lifecycle** — who owns retention/cleanup of `workflow_s3_uri` journals (reaper?
   release pipeline?), and is per-checkpoint S3 push acceptable cost for long sweeps? (§4.2–4.4)
4. **Cold-restart window** — accept the create-run→persist-runId gap as-is, or pre-allocate a
   runId before the pod starts so it is always persisted? (§4.2)
5. **Double-dispatch lease** — is a `workflow_lease` field the right mechanism, and which
   reclaim paths (gardener, stale-claim) must honor it? (§4.5)
6. **Gardener autonomy** — may the four-signal classifier *override* an absent `exec_hint` and
   force dynamic-workflow, or only ever *recommend* in prompt text? (§5.1, §5.3)
7. **Concurrency coordination** — does a dynamic-workflow step's `min(16, cores-2)` internal cap
   need to be coordinated with gardener `run_budget`/`max_runs` so one host pod doesn't starve
   sibling steps of cluster capacity? (§1, §5.4)

---

*Method note: this doc was produced by a dynamic workflow (the very primitive it analyzes) — 5
parallel dimension-design subagents → 1 synthesis → 1 adversarial reviewer — then reconciled
against the live `kd`/scheduler surface by the host agent. The adversarial pass caught six real
errors in the synthesis draft (invented CLI flags, an `expand --values`-vs-scalar conflation, a
prefer-vs-sum cost inconsistency, an unverified reaper guarantee, a mis-timed gardener signal, and
a backwards follow-up-ordering); all are corrected above and called out inline.*
