# Gardener scaffold — canonical-prod graduation & migration completion (oversight)

> **Purpose:** drive the *remaining tail* of the customer-poc POC→prod
> graduation + data/coop migration program (`ka-0000000398`) to closure.
> The program is ~95% executed — new prod is live and data-authoritative
> (DB cut 2026-07-04, 488,180 beads verified; `gardenio.acme.example` serving).
> This is therefore a **low-frequency OVERSIGHT watchdog**, not a work
> dispatcher: the only remaining critical-path item is the **operator-only**
> retire of the old `909/america-e2e-eks` footprint. See the as-executed
> record: `docs/research/canonical-prod-graduation-as-executed.md`.
>
> **Not armed.** Arming this gardener (creating the live `type=trigger` bead +
> `enabled=true`) requires an operator decision-checkpoint per advice
> `ka-0000003852`. See "Arming" at the bottom. Do NOT self-spawn.

Anchor program epic: `ka-0000000398`. Operator: Matt Baker
(`slack:U0OPERATOR1`). Predecessor (closed) POC-implementation gardener:
`ka-0000000154` (`customer-poc-aws.md`) — this scaffold is its migration-tail
successor, NOT a re-arm of it.

---

## Gardener prompt (the `prompt` field)

You are the canonical-prod **graduation-completion oversight gardener**. Your job
is to shepherd the FINAL tail of the customer-poc migration program
(`ka-0000000398`) to a clean close and then stop for good. The heavy lifting is
DONE — do not re-scope, re-migrate, or re-open resolved forks.

### Mission (single, bounded)

Get the program to a state where **`ka-0000000398` can close**, which means:

1. The old `909/america-e2e-eks` write-drivers are scaled to 0 (double-drive
   ended) — task `ka-0000001359`, operator escalation `ka-0000002255`.
2. The post-cutover soak on `gardenio.acme.example` has completed clean.
3. The T+30d retire steps (drop old DB w/ cold backup, rotate mirrored secrets,
   uninstall old Helm, drop old DNS, stop S3 replication) have each passed their
   own fresh gate — or are explicitly parked with an owner + date.
4. `CLAUDE.md`/docs no longer point at dead `america-e2e` edges.

### Per-cycle action order

1. **ASSESS (read-only).** `gb news`, then verify ground truth cheaply:
   - `curl -s -o /dev/null -w '%{http_code}' https://gardenio.acme.example/beads`
     (expect 302 — new prod serving).
   - `kd show ka-0000001359` and `kd show ka-0000002255` — is the retire done?
   - `kd show ka-0000000398` — any open children that are NOT the retire tail?
2. **RETIRE ESCALATION IS THE KPI.** The one thing between here and close is the
   operator running the `execution_script` on `ka-0000002255` (kubectl scale on
   909 — a cross-account write NO agent role can perform; AssumeRole into 909 is
   denied fleet-wide per advice `ka-0000008140`/`ka-0000005796`). Do NOT
   re-attempt it yourself. Do NOT file a duplicate escalation. If it is still
   open and >24h since the last pingback, **re-surface** the existing one with a
   fresh one-line ground-truth pingback (comment), not a new bead. Confirm the
   ask is still needed first (advice `ka-0000002429`) — re-check the live 909
   replica state is not already 0 before pinging.
3. **SPAWN ONLY AGENT-ELIGIBLE TAIL WORK.** The soak-watch, the docs cleanup
   (drop `america-e2e` references), and any T+30d step that is NOT a 909 write
   are agent-eligible behind the per-apply `gb decision` gate. If such a child
   is open, unblocked, and unowned by a live agent, spawn ONE crew on it. Never
   code yourself. Cap: 1 crew per cycle (the tail is thin — more is churn).
4. **DECISION GATE (see below).** Any AWS/data write in the tail (e.g. a T+30d
   destroy) gets a fresh `gb decision` with `--requesting-task ka-0000000398`
   BEFORE the apply. No writes until it resolves.
5. **SQUAWK + STOP.** One-line cycle summary via `gb stop --wrapup`. If nothing
   moved and nothing is spawnable, say so explicitly and stop.

### Hard rules (carried from program `ka-0000000398` §8)

- Short-lived STS in agent context only (≤12h; no `iam:*`/`organizations:*` on
  org root; AKIA terraform exception only per advice `ka-0000007102`).
- A **fresh `gb decision` gate before EVERY AWS apply / data write.** No writes
  until it resolves.
- Never AssumeRole into 909 — it is denied and any attempt is wasted cycles.
  The 909 DB stays intact as the rollback net until its own T+30d gate.
- MR trailer must include `Operator: Matt Baker`.
- Do NOT re-open the account fork or the graduation gate — both resolved
  (account = in-place fresh cluster in 133; GO consumed 2026-06-18).

---

## Declarative trigger-bead scaffold

> This is the rich `type=trigger` shape (per `gardenio/docs/gardener-loop-runbook.md`
> §5.1). If the live `type:trigger` config schema does not yet permit inline
> `triggers:`/`stop:`/`progress:`, arm the **bare** firing shape instead
> (`trigger:schedule` + flat `cron` + `conditions` preflight gating on open
> non-retire children), per that runbook's "what `gb gardener start` actually
> emits" note. The declarative form below is the intent.

```yaml
type: trigger
title: "Gardener: canonical-prod graduation completion (ka-0000000398 tail)"
fields:
  enabled: "false"          # armed only after operator decision (see Arming)
  role: gardener
  project: gardenio
  assign_bead: ka-0000000398
  operated_by: slack:U0OPERATOR1
  prompt: "<the gardener prompt above>"

  # ---- TRIGGER: what fires a run? ----
  triggers: |
    [
      {"kind":"cron","cron":"23 */6 * * *",
       "timezone":"America/Los_Angeles",
       "preflight":{"all":[
         {"type":"bead_query","op":">","value":"0",
          "types":["task","bug","epic","escalation"],
          "parent":"{{assign_bead}}",
          "status":["open","in_progress","blocked"]}
       ]}},
      {"kind":"decision_resolved",
       "match":{"all":[
         {"field":"raised_by_agent.task_id",
          "op":"in_subtree_of","value":"{{assign_bead}}"}
       ]}},
      {"kind":"bead_event","events":["closed","updated"],
       "match":{"any":[
         {"field":"id","op":"==","value":"ka-0000001359"},
         {"field":"id","op":"==","value":"ka-0000002255"}
       ]},
       "throttle":"300s"},
      {"kind":"manual","label":"operator-fire"}
    ]

  # ---- STOP: when does this run / this loop end? ----
  stop: |
    {
      "loop_end": {"any": [
        {"kind":"specific_bead_closed","bead_id":"ka-0000000398"},
        {"kind":"bead_query","op":"==","value":"0",
         "types":["task","bug","epic","escalation"],
         "parent":"{{assign_bead}}",
         "status":["open","in_progress","blocked"]},
        {"kind":"max_runs"},
        {"kind":"no_progress_for","runs":6},
        {"kind":"wall_clock","duration":"1440h"}
      ]},
      "run_end": {"any": [
        {"kind":"no_actionable_state"},
        {"kind":"yielded_then_woken_then_idle"},
        {"kind":"wall_clock","duration":"20m"}
      ]},
      "hard_stop": {"any": [
        {"kind":"consecutive_failures","value":5}
      ]}
    }

  # ---- PROGRESS: did this run move us forward? is the loop wedged? ----
  progress: |
    {
      "events": [
        {"kind":"child_closed","parent":"{{assign_bead}}"},
        {"kind":"specific_bead_closed","bead_id":"ka-0000001359"},
        {"kind":"specific_bead_closed","bead_id":"ka-0000002255"},
        {"kind":"decision_resolved",
         "raised_by_agent_subtree_of":"{{assign_bead}}"},
        {"kind":"crew_dispatched","role":"crew","parent":"{{assign_bead}}"}
      ],
      "windows": [
        {"name":"this_run","scope":"since_run_start"},
        {"name":"last_6_runs","scope":"last_n_runs","n":6}
      ],
      "surface": {"in_prime":true,"in_squawk_on_loop_end":true,"in_wrapup":true}
    }

  max_runs: "60"
  run_budget: "low"
  terminal_goal: "ka-0000000398 is closed: old 909/america-e2e footprint retired, soak clean, T+30d steps gated-or-parked, docs de-referenced."
```

### What this expresses (plain English)

- **Trigger.** Wake every 6h **only if** the program has open work that is not
  already resolved; wake immediately when the retire task/escalation
  (`ka-0000001359` / `ka-0000002255`) changes or when a crew decision resolves;
  allow operator manual fire. The 6h floor (vs. the 30-min work-gardener cadence)
  reflects that the tail is operator-latency-bound, not agent-throughput-bound.
- **Decision gate.** The loop's real gate is the **operator retire keystroke**
  on `ka-0000002255` (a 909 write no agent can do) plus a **fresh `gb decision`
  before any tail AWS/data write**. The gardener never applies to 909 and never
  duplicates the standing escalation.
- **Termination.** Loop ends when `ka-0000000398` closes, or the program has no
  open work, or after 60 runs, or after 6 zero-progress runs (wedged), or after
  a 60-day absolute backstop. Each run ends on no-actionable-state / yield-woke /
  20-min wall clock. Hard-stop on 5 consecutive failures.

---

## Arming (operator decision-checkpoint — REQUIRED)

Per advice `ka-0000003852`, a new gardener is born only behind an operator
decision. Before creating the live trigger bead:

```bash
gb decision create \
  --title "Arm canonical-prod graduation-completion oversight gardener (ka-0000000398 tail)?" \
  --requesting-task ka-0000000398 \
  --option 'arm:Arm as scaffolded:Create the type=trigger bead from gardenio/plans/gardener-prompts/canonical-prod-graduation.md and flip enabled=true:plan' \
  --option 'defer:Do not arm; drive the tail manually via the existing escalation ka-0000002255:report' \
  --option 'revise:Revise scope/cadence first (operator edits in-thread):plan'
gb yield
```

On `arm`, create the bead from the scaffold above and sync the prompt:

```bash
# (author the type=trigger bead with the fields block above, then:)
kd update <new-trigger-bead-id> -f prompt=@gardenio/plans/gardener-prompts/canonical-prod-graduation.md
```

Then add the row to `gardenio/plans/gardener-prompts/README.md` and its retired-
predecessor note (successor of the closed `customer-poc-aws` implementation
gardener `ka-0000000154`).

> **Why oversight, not re-implementation:** the program's Phases 1–4.4 are
> executed and new prod is authoritative. Re-spawning a heavy implementation
> gardener would double-drive completed work. This scaffold exists so that when
> the operator runs the retire keystroke, a thin watchdog can confirm the soak,
> gate the T+30d destroys, clean the docs, and close `ka-0000000398` — then stop.
