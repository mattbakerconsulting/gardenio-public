# Haiku Phase-0 Shake-Out Runbook (WS-1)

_Operator runbook for the Haiku phase-0 model experiment._

- **Bead:** ka-0000001778 (WS-1)
- **Epic:** ka-0000004375 (Build first-mate v0 + Haiku phase-0 + retire-sweep)
- **Decision:** ka-0000000030 (build-fm + retire-sweep, Haiku confirmed)
- **Design source of truth:** [`first-mate-fleet-role-design-2026-06-11.md`](first-mate-fleet-role-design-2026-06-11.md) §9
- **Operator:** Matt Baker

---

## 1. What this is

WS-1 pins a single low-blast-radius meta-gardener — the **meta-scorer**
("gardener of gardeners", `ka-0000001271`) — to a **Haiku-backed account** for a
bounded window, then measures whether Haiku can drive a full agentic
supervisory loop as well as the Opus baseline (§9.2 below).

We have **never run a full agent session on Haiku** in this fleet — Haiku today
only powers narrow single-shot sub-tasks (namegen, health probes, bridge
summaries). The meta-scorer is the safe guinea pig because it has **zero
autonomous destructive authority** (§5.3 guardrail): every config edit and stop
it proposes is gated behind an operator `gb decision`. The worst a confused
Haiku run can do is file a proposal the operator declines.

## 2. The mechanism — config only, NO engine change

Model selection is **account-driven**: the controller resolves an agent pod's
model from its account bead (`applyAccountConfig` sets `ANTHROPIC_MODEL` /
`ANTHROPIC_CUSTOM_HEADERS` from the account's `model` + `litellm_key`,
`podspec.go`). A **role→account pin** (`resolveRoleAccount`) lets a role override
the project's account — exactly how `fsm-triage` is pinned to `litellm-bedrock`
today.

So the Haiku pin is a **config composition**, not code:

```text
gardener ka-0000001271  (field: role = phase0-haiku)
        │
        ▼
role bead  phase0-haiku  (field: account = first-mate-haiku)
        │
        ▼
account bead  first-mate-haiku  (field: model = claude-haiku-4-5)
        │
        ▼
LiteLLM alias claude-haiku-4-5  →  anthropic/claude-haiku-4-5
```

When the scheduler spawns the meta-scorer (`action: spawn_agent`,
`role: phase0-haiku`), the pod inherits the Haiku model automatically — "out of
the box", no per-spawn flag. `selectAccount` preserves the role pin: a
role-pinned account always wins over the project's round-robin pool
(`account_spread.go`), so the spread path never redirects the meta-scorer off
Haiku.

The config beads were created/designated in the daemon (they are runtime config,
not source-checked-in). This MR carries the **repo-side artifacts** that make the
pin reproducible, regression-locked, and reversible:

- `TestApplyAccountConfig_Phase0HaikuRolePin` (`podspec_test.go`) locks the
  resolution chain `phase0-haiku → first-mate-haiku → claude-haiku-4-5` and
  proves the role pin overrides a project Opus account.
- This runbook documents the bounded window and the kill-switch.

### Live config beads (for reference / re-seed)

| Bead | Type | Title | Key fields |
|---|---|---|---|
| `ka-0000001271` | trigger | Meta-Gardener twice-daily scoring | `role: phase0-haiku` |
| `ka-0000002357` | role | `phase0-haiku` | `account: first-mate-haiku`, `session_resume: false` |
| `ka-0000002464` | account | `first-mate-haiku` | `model: claude-haiku-4-5`, `provider: anthropic`, `base_url: https://litellm.app.devops.acme.example` |

## 3. The pin is bounded AND reversible (kill-switch)

This is the **single fallback lever** the design calls for (§9.1.3): if phase-0
says Haiku underperforms, repoint the role at a Sonnet/Opus account. One config
edit, no code, no re-design.

### Kill-switch — revert the gardener to its default (Opus) model

**Fastest revert** (one command — flip the gardener's role back to the default
gardener role, which carries no account pin and therefore falls back to the
project's Opus-backed account):

```bash
kd update ka-0000001271 --field role=gardener
```

**Alternative revert** (keep the role, repoint it at a non-Haiku account — useful
if other beads also use `phase0-haiku`):

```bash
kd update ka-0000002357 --field account=<sonnet-or-opus-account>
```

Either change takes effect on the **next spawn** of the meta-scorer (cron
`23 7,19 * * *`, America/Los_Angeles). Already-running pods are unaffected; the
worst case is one more Haiku run before the next tick. No pod kill is required.

### Bounded window

The experiment is naturally bounded by the meta-scorer's own loop budget (60
runs ≈ 30 days twice-daily, see the gardener prompt's `LOOP EXIT`). The operator
ends the window early at any time with the kill-switch above. Record the
window start/end and the §9.2 measurements as comments on `ka-0000001778`.

## 4. Measurement track (§9.2) — Haiku vs Opus baseline

Score Haiku's runs against an Opus baseline on the same gardeners:

| Dimension | Pass bar |
|---|---|
| **Loop integrity** | Completes its run: claims, reasons over the subtree, files proposals, self-blocks correctly — no tool-call malformation, no premature stall. |
| **Verdict parity** | On a labeled set of gardeners, Haiku's trajectory verdicts match the Opus baseline / operator ground truth ≥ threshold (start ~80%, tune). |
| **Proposal quality** | The `gb decision` proposals it authors (re-plan / succession / retire) are ones the operator would accept. |
| **Scope discipline** | Stays inside the supervisory remit — does NOT start doing crew work, editing code, or over-escalating. |
| **Cost / latency** | Per-run cost and wall-clock vs. Opus — quantify the "keep things fast/cheap" win. |

**Outcomes & fallbacks:**

- **Pass** → Haiku becomes the shipped default mate model (the §9.1 role pin
  carries into first-mate v0, WS-2).
- **Partial** (cheap sensing fine, weak on authoring re-plans) → per-verdict
  model split (OQ6): Haiku for routine ON-TRACK/BLOCKED sweeps, escalate to
  Sonnet/Opus to author a THRASHING/DRIFTING re-plan.
- **Fail** → kill-switch back to Sonnet/Opus (§3); first mate still ships, model
  TBD.

## 5. Coordination note (WS-B)

If phase-0 uses the meta-scorer as the Haiku guinea pig, keep the
resumer-watchdog gardener on its current model until its own go-live is decided —
so the model experiment does not muddy the watchdog observability metrics
(design §9.3).
