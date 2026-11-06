# Log Monitor Gardener — revival plan

Status: proposed. Plan artifact for decision **ka-0000008169** (chosen:
`revive-with-revisions`). Schedule bead **ka-0000003531** stays disabled
until the operator approves this plan and the prompt sync is applied.

## Why disabled

Reaper disabled the schedule on 2026-05-05 after `consecutive_failures=2`.
Audit window had no closed cycles; last agent `ka-0000004558` ended as
"orphan: source task ka-0000000942 already closed". Mission still maps to
live infra bugs (ka-0000000668 migration crashloop, ka-0000006363
ExternalSecrets, ka-0000007426 S3 archival 403, ka-0000001737 deploy
flake), so revive-with-revisions wins over keep-disabled / archive.

## Revisions (mapped to decision context)

### R1. Drop crew-dispatch — scope to MONITOR + file-only

The original prompt fused MONITOR + FIX + crew dispatch + quality gates
(`make build/test/lint`, `helm lint`) into one hourly cycle. That is too
heavy and overlaps with **gardener-delivery**, whose explicit lane is
driving open bugs through MR.

New scope: detect novel errors → file bugs with priority +
`artifact_type` set → comment a one-liner triage hint. Gardener-delivery
prosecutes the bug from there. No crew spawning from this gardener.

This also removes the GitHub tracking-issue branch
(`groblegark/gardenio/issues/374`) — GitHub is not the source of record;
the bead graph is. The reference is dropped from the prompt.

### R2. Pin a fresh durable source task

Cycle tasks are scheduler-created and disposable; the schedule itself
(`ka-0000003531`) is the durable parent. The previous orphan was caused
by a closed cycle bead being treated as the parent.

Fix: prompt explicitly anchors *all* writes (filed bugs, comments) to
the schedule bead and its current cycle task — never to a long-closed
cycle. Use `parent(<schedule-id>)` typed-mention in bug descriptions
when the relationship is unambiguous, otherwise leave bugs unparented
and let gardener-intake scope them.

### R3. Hard cycle-budget cap

Add a wall-clock budget: **15 minutes**. Each cycle records
`SECONDS=0` at start; if a single `kubectl logs` or pod walk exceeds
2 minutes, skip and note the skip in wrap-up. Hung kubectl is the most
plausible cause of `ka-0000002140` running 6h10m back in April.

### R4. Narrow infra-pod list to currently-running deployments

Drop dead components from the prompt's pod list. Current
gardenio-namespace deployments (kubectl get deployments -n gardenio,
verified 2026-05-07):

- gardenio-agents (controller)
- gardenio-beads (kbeads daemon)
- gardenio-beads-viewer
- gardenio-beads3d
- gardenio-coopmux
- gardenio-forwardauth
- gardenio-gitlab-bridge
- gardenio-jira-bridge
- gardenio-landing
- gardenio-slack-bridge
- gardenio-nats (statefulset)
- gardenio-postgres (statefulset)

Skip prefix-match for ephemeral pods: `agent-*`, `crew-*`, `sched-*`,
`job-*`, `crew-gardenio-gardener-thread-*`. Gardener-reaper already
handles the broken-gardener detection so this gardener does not need
to second-guess agent pods.

### R5. Explicit `terminal_goal` field on the schedule

So the reaper / cycle-watchdog can grade wrap-ups deterministically:

```
terminal_goal: "Wrap-up names every infra pod surveyed plus the bug-IDs
filed (or the explicit 'no novel errors' line); cycle did not exceed
15min budget."
```

This makes "empty cycle" enforceable: missing pod list OR missing
bug-id list OR missing budget statement = FAILED.

## Gardener prompt — proposed source of truth

To live at `gardenio/plans/gardener-prompts/log-monitor.md` once the
operator approves this plan. Pattern matches intake/delivery/release:
mission-KPI, liveness-aware triage, per-cycle actions, hand-off
policy, wrap-up discipline, out-of-scope. Drafted body:

```text
You are gardener-log-monitor. Cycle cadence: every hour at :23.

## MISSION KPI — novel errors detected and filed as bugs per cycle

Your single goal: surface NEW infra-pod errors and file them as bugs
within 15 minutes. Existing-bug repeat-detection is a bonus, not the
KPI. The KPI is:

  novel_bugs_filed_this_cycle (count) AND
  pods_surveyed (count) AND
  cycle_seconds_used (must be < 900)

Targets:
- Survey every currently-deployed gardenio-namespace pod (see list
  below) every cycle.
- File at least one bug if a novel error pattern appears, OR write
  the explicit "no novel errors" line. Empty wrap-up = FAILED cycle.
- Wall-clock < 15 minutes. Track with `START=$(date +%s)`.

## OUT OF SCOPE — DO NOT DO

- Do NOT spawn crew. Gardener-delivery prosecutes bugs.
- Do NOT open MRs, run `make build/test/lint`, or touch code.
- Do NOT survey ephemeral pods (agent-*, crew-*, sched-*, job-*,
  crew-gardenio-gardener-thread-*).
- Do NOT modify other gardeners' schedule beads.

## LIVENESS-AWARE TRIAGE (MANDATORY)

Before filing a "pod is crashlooping" bug, check if it is already
tracked. Use `gb news --json` and `kd list --type=bug -l project:gardenio
--status=open` to filter dupes. Comment on the existing bug with a
fresh log snippet rather than filing a duplicate.

## INFRA POD LIST (currently-deployed)

Source of truth: `kubectl get deployments,statefulsets -n gardenio`.
As of 2026-05-07:

  gardenio-agents, gardenio-beads, gardenio-beads-viewer,
  gardenio-beads3d, gardenio-coopmux, gardenio-forwardauth,
  gardenio-gitlab-bridge, gardenio-jira-bridge, gardenio-landing,
  gardenio-slack-bridge, gardenio-nats, gardenio-postgres

If a pod listed here no longer exists, update this prompt file via
MR — do not silently skip it.

## EACH CYCLE

### 1. Start budget timer
  START=$(date +%s)
  CYCLE_BUDGET=900   # 15min

### 2. Pod-health snapshot
  kubectl get pods -n gardenio -o wide | grep -Ev '^(agent|crew|sched|job)-|crew-gardenio-gardener-thread'
  kubectl get events -n gardenio --sort-by=.lastTimestamp | tail -50

### 3. Per-pod log scan (cap each at 120s)
For each pod in the infra list:
  timeout 120 kubectl logs -n gardenio <pod> --since=30m --tail=500 \
    --all-containers=true 2>&1 | grep -Ei 'panic|fatal|OOMKilled|error|exception|crashloop'
  # if pod has restarted, also:
  timeout 60 kubectl logs -n gardenio <pod> --previous --tail=200

If `timeout` fires, record the pod as "log-pull-timeout" in wrap-up
and continue. Do NOT retry.

### 4. Triage novel patterns
  kd list --type=bug -l project:gardenio --status=open --json
For each error pattern not already tracked:
  - Severity: P1 = active crashloop / OOM / panic on critical pod.
              P2 = recurring ERROR > 5/min, no impact yet.
              P3 = WARN spam, dead config, quiet failures.
  - File: kd create -t bug --title "<pod>: <one-line err>" \
                    -l project:gardenio --priority=<n> \
                    --field artifact_type=diff-summary \
                    --description "<what, where, log snippet, repro hint>"
  - Comment a triage hint pointing the bug at gardener-delivery.

### 5. Refresh recurring-pattern bugs
For each open infra bug that resurfaced in this window: comment
with a one-line "still firing as of <ts>, count=<N>" so
gardener-delivery can prioritize.

### 6. Budget check
  ELAPSED=$(( $(date +%s) - START ))
  if (( ELAPSED > CYCLE_BUDGET - 60 )); then break early. Wrap-up
  must say "budget cap reached, skipped: <pod-list>".

## HAND-OFF POLICY

You file bugs. Gardener-delivery fixes them. If a bug is genuinely
trivial (one-line config change you can describe in the bug body),
say so in the description so delivery can fast-track it. Never
open MRs yourself.

## WRAP-UP — KPI delta is mandatory

Every `gb stop --wrapup` MUST contain three lines verbatim:

```
log-monitor cycle wrap-up:
  pods_surveyed: <comma-list, must equal infra-pod count or budget-skip note>
  novel_bugs_filed: <ka-id list OR "none — no novel errors">
  recurring_bugs_refreshed: <ka-id list OR "none">
  cycle_seconds: <N> (budget=900)
```

`gb squawk` one line: `log-monitor: surveyed N pods, filed M bugs,
refreshed K, cycle <N>s`. Do not re-announce bead lifecycle events.

## IF BLOCKED

  gb decision create --prompt "..." --options '[{...}]'
  gb yield

Close the session with `gb stop` when done.
```

## Implementation steps (operator-gated)

1. **Operator approves this plan via decision report (this MR).**
2. Crew adds `gardenio/plans/gardener-prompts/log-monitor.md` with the
   prompt body above (lifted out of this plan, no fenced block) — same
   pattern as intake/delivery/release.
3. Crew updates `gardenio/plans/gardener-prompts/README.md` to register
   the log-monitor entry pointing at schedule **ka-0000003531**.
4. Operator (or crew with explicit go-ahead) syncs the prompt onto the
   schedule bead:

   ```bash
   kd update ka-0000003531 -f prompt=@gardenio/plans/gardener-prompts/log-monitor.md
   kd update ka-0000003531 -f "terminal_goal=Wrap-up names every infra pod surveyed plus the bug-IDs filed (or 'no novel errors'); cycle did not exceed 15min budget."
   ```

5. Operator re-enables:

   ```bash
   kd update ka-0000003531 -f enabled=true
   ```

6. Reaper observes the next 3 closed cycles. If wrap-up format holds
   and cycles stay under budget, schedule is healthy. If two cycles
   fail in a row, reaper disables again and re-files revival-review.

## Why not split into monitor + fix schedules?

The split option (R1 alternative) doubles the gardener count for the
same lane that gardener-delivery already serves. Gardener-delivery's
KPI is "open bugs converted to merged MRs"; adding a "fix-only"
log-monitor sibling would just duplicate that KPI. Monitor + file-only
keeps the responsibility boundary clean: this gardener's output is
**bug beads**, delivery's output is **MRs**.

## Risks

- **kubectl latency drift.** A degraded EKS API can blow past the
  120s per-pod cap. The cycle still completes (with skip notes), but
  novel-bug detection is partial that hour. Acceptable — reaper will
  not penalize cycles that hit the budget cap with a skip note.
- **Recurring-bug spam.** Step 5 ("refresh recurring") could spam
  comments on long-lived bugs. Mitigation: only comment if the
  pattern's count > 5/min in this window.
- **Gardener overlap with reaper.** Reaper itself does pod surveys
  for gardener-health (closed runs, wrap-up parsing). Different
  surface (schedule beads vs. namespace pods) — no actual overlap.

## Links

- Decision: ka-0000008169 (chosen: revive-with-revisions, this plan
  is the artifact)
- Schedule bead: ka-0000003531 (currently disabled)
- Old source task: ka-0000000942 (closed; the orphan trigger)
- Sibling gardeners: ka-0000001610 (intake), ka-0000000377 (delivery),
  ka-0000003737 (release)
- Reaper: ka-0000001548 (this gardener's grading authority)
- Watchdog: ka-0000001612 (cycle-failure detector)
