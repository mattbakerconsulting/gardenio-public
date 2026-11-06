# Gardener Resume — Observability Layer + Gardener Succession (Plan)

> **Decision:** ka-0000006918 — operator chose *observability-first* (land the
> resume metrics + /gardeners surfacing so we can SEE resumes before going live
> with the watchdog), AND added a new requirement: **gardener succession** —
> gardeners should create successors before completing their goals (research →
> impl → improvement → regression-checking).
> **Origin:** ka-0000005353 · **Engine MR:** !3948 · **Monitoring bead:** ka-0000005415
> **Operator:** Matt Baker

## Sequencing (revised per the decision)

1. **Observability** (this plan §1) — land first; nothing live until we can see it.
2. **Gardener succession** (this plan §2) — new ask; design + implement.
3. **Resumer watchdog go-live** — deferred until §1 is visibly working; revisit
   via a fresh operator decision (the prompt + advice ka-0000004810 already exist).
4. **Live verification** (ka-0000005415) — the controlled resurrection proof,
   now also covering succession.

---

## §1 — Observability layer

Goal: make every resume / escalate decision **visible** — in Prometheus and on
the `/gardeners` page — so an operator can watch the master-gate working before
we automate revival.

### 1a. Metric: `gardener_resume_total`

A `CounterVec` in `internal/metrics/metrics.go` (mirrors the existing
`*Vec` + `Register` pattern there), incremented from `gardenerResumeOrEscalate`
in `internal/bridge/gardener_evaluator.go`:

```
gardener_resume_total{gardener_id, action, reason}
  action ∈ {rearm, escalate}
  reason ∈ {max_runs, no_progress_for, wall_clock, consecutive_failures}
```

Plus a gauge `gardener_resumes_without_progress{gardener_id}` set on each persist,
so a gardener creeping toward its resume budget is graphable. The bridge already
holds a `*metrics.Metrics` handle (see `init.go`); thread it into the evaluator
the same way the tracker reaches other deps.

### 1b. Surface resume bookkeeping on `/gardeners`

The gardener block already persists `resumes_without_progress`,
`last_resume_reason`, `resume_budget` (engine MR !3948). Add to
`cmd/beads-viewer/gardener_page.go`:

- a **Resume** column/pill per gardener: `resumes_without_progress / resume_budget`,
  colored as it approaches the cap; `last_resume_reason` on hover.
- a **stuck badge** when the gardener carries the `gardener-stuck-escalate` label
  (engine escalation awaiting the watchdog/operator).
- the most recent run's `goal_satisfied` (✅ declared-done vs ⏳ still-going) so
  "why is this gardener still open" is answerable at a glance.

### 1c. Tests

- `metrics_test.go`: counter increments on rearm vs escalate with correct labels.
- `gardener_page_test.go`: renders the resume pill + stuck badge from a gardener
  bead carrying the fields (follow the existing `gardener_test.go` table style).

### 1d. Status — LANDED (ka-0000000971, WS-B)

§1 is complete and shipped. Note one design correction discovered at build time:
the metric lives in `internal/bridge/gardener_resume_metrics.go` (a `promauto`
`CounterVec` + `GaugeVec` on the **default** registry), not in
`internal/metrics/metrics.go`. That is the right home — `gardenerResumeOrEscalate`
runs inside the bridge `TriggerEngine`, so the metric is produced in whichever
binary fires gardeners (`trigger-engine`, `gitlab-bridge`), and promauto on the
default registry is the pattern every other bridge metric uses
(`operator_activity_metrics.go`).

Pieces, all present and tested:

- **Metric** — `gardener_resume_total{gardener_id,action,reason}` (action enum
  `rearm|escalate`; reason enum `max_runs|no_progress_for|wall_clock|consecutive_failures|other`)
  plus the `gardener_resumes_without_progress{gardener_id}` gauge. Incremented from
  `gardenerResumeOrEscalate` via `recordGardenerResume` / `setGardenerResumesWithoutProgress`.
- **/gardeners surfacing** — `gardenerView` carries `ResumesWithoutProgress`,
  `ResumeBudget` (0 → default 5), `LastResumeReason`, `StuckEscalated`; the
  detail template renders the **Resumes fruitless/budget** pill (warn-colored at
  the cap) and the **⚠ STUCK — ESCALATED** badge from the `gardener-stuck-escalate`
  label; goal-state is re-derived live (`GardenerGoalState`).
- **Scrape wiring (the gap WS-B closed)** — `trigger-engine` and `gitlab-bridge`
  previously served only `/healthz`,`/readyz`,`/webhooks/`, so the counter
  incremented in-process but was **never scraped**. Both binaries now expose an
  internal-only `/metrics` (`buildMetricsServer`, default gatherer, `:9092`,
  `METRICS_LISTEN_ADDR`), and the Helm deployments carry
  `prometheus.io/scrape` pod annotations + a `metrics` container/service port —
  mirroring slack-bridge. This is what makes the Prometheus half real.
- **Tests** — `gardener_resume_metrics_test.go` (label canonicalization, increment,
  and `TestGardenerResumeMetricsOnDefaultRegistry` proving the families land on the
  served default registry); `gardener_test.go::TestGardenerViewFromBead_ResumeSurfacing`;
  `gardener_page_test.go`; `cmd/trigger-engine/main_test.go::TestBuildMetricsServer_ServesDefaultGatherer`.

---

## §2 — Gardener succession (new requirement)

> *"We'd like a mechanism for gardeners to create successors before they complete
> their goals — they should be encouraged and guided to do so. A research gardener
> can give way to impl, to improvement, to regression-checking."* — Matt

### The idea

A gardener's `terminal_goal` is often **one phase of a pipeline**. When a research
gardener declares done, the *work* isn't done — it's ready for the **next phase**.
Today "done" is terminal and the chain dies. Succession lets a gardener **hand the
baton**: on declaring its own goal met, it spawns (or arms) the next-phase
gardener, with provenance, so the pipeline self-propagates.

This composes cleanly with the master gate: `goal_satisfied=="yes"` already means
"this phase is done" — succession hooks exactly there.

### 2a. Declarative successor on the gardener bead

A new optional gardener-block field:

```json
"successor": {
  "phase": "impl",                       // human label for the next phase
  "terminal_goal": "…next phase goal…",  // the successor's goal
  "prompt_advice": "kd-…",               // advice bead carrying its prompt
  "epic": "{{same|kd-…}}",               // epic to drive (default: same epic)
  "inherit": ["project","slack_channel","operated_by"],
  "when": "goal_satisfied"               // trigger: on this gardener's done-declaration
}
```

On terminal `goal_satisfied` close (in `gardenerTerminate`, when a `successor` is
present), the engine — or a small succession helper — **creates the successor
gardener** (type=trigger, role=gardener), pins it to the epic, stamps a
`succeeds: <predecessor-id>` typed mention for provenance, and enables it. The
predecessor closes as done; the successor wakes on its own preflight.

### 2b. Guidance (the "encouraged and guided" half)

Mechanism alone isn't enough — gardeners must be *prompted* to think about
succession. Two cheap, high-leverage touchpoints:

- **Wrap-up prompt nudge:** when a gardener is about to declare `goal_satisfied:yes`,
  the gardener prompt (and the `gb stop` wrap-up guidance) asks: *"Is this the end
  of the whole effort, or the end of a phase? If a phase, define your successor
  (next goal + prompt) so the pipeline continues."* Lands as advice + a line in
  the gardener-loop runbook.
- **Meta-gardener scorer dimension:** the scorer (ka-0000007687) gains a light
  check — a gardener that closed `goal_satisfied` on a phase-shaped epic *without*
  a successor, while sibling/downstream work clearly remains, gets a "consider
  succession" advisory. Not punitive; a nudge.

### 2c. Succession vs. resume (how they interact)

- **Resume** = "goal NOT done, you stopped prematurely → come back."
- **Succession** = "goal IS done, but the next phase should begin → spawn heir."

They're the two halves of "the pipeline keeps moving until the *whole effort* is
done." The master gate is the shared hinge: premature → resume; terminal-with-
successor → succeed; terminal-no-successor → truly stop.

### 2d. Open design question for build time

Where succession executes: (a) **engine-side** in `gardenerTerminate` (zero agent
latency, but the engine gains "create a gardener" authority — bigger blast radius),
vs. (b) **agent-side** — the gardener itself spawns its successor in its final run
before `gb stop` (keeps the engine pure; relies on the prompt nudge). Leaning (b)
for v0 (guidance-driven, no new engine authority), with (a) as a future
convenience once the pattern is proven. Resolve at implementation start.

---

## Build order

1. §1a metric + §1b /gardeners surfacing + §1c tests — **observability, ships first.**
2. §2a successor field + §2b guidance (prompt nudge + runbook + scorer advisory).
3. Live verification (ka-0000005415): controlled resurrection **and** a
   research→impl succession hand-off, both documented as proof.
4. Resumer-watchdog go-live — fresh operator decision once §1 is visibly working.

## Source map

| Concern | File |
|---|---|
| Metric definitions | `gardenio/controller/internal/metrics/metrics.go` |
| Resume/escalate emit point | `gardenio/controller/internal/bridge/gardener_evaluator.go` |
| /gardeners page | `gardenio/controller/cmd/beads-viewer/gardener_page.go` |
| Gardener terminate (succession hook) | `gardenio/controller/internal/bridge/gardener_evaluator.go` |
| Wrap-up guidance | `gardenio/controller/cmd/gb/wrapup.go` + gardener prompt advice |
| Scorer succession advisory | `gardenio/docs/gardener-effectiveness-rubric.md` |
