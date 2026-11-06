# Per-Project Gardener Defaults — Design Spec

**Bead:** ka-0000000304  ·  **Status:** design (do NOT implement under the spec task)
**Author:** gardenio-spec-perproject-gardener-34i  ·  **Date:** 2026-08-21

> **This is DESIGN only.** It specifies a per-project layer for gardener /
> schedule-trigger defaults, recommends a storage mechanism, and defines
> precedence + migration. Implementation is scoped into a follow-up epic
> (§9), not built here.

---

## 1. Problem

Gardener and schedule-trigger parameters (cron cadence, cooldown, max-concurrent,
timezone, role, model, wall-clocks, …) come from exactly **two** places today:

1. **Global binary defaults** baked into the CLI flag registration:
   - `gb gardener start` — `--cron ""` (deterministic staggered 30m cron seeded
     from the epic id), `--max-concurrent 3`, `--cooldown 5m`
     (`gardenio/controller/cmd/gb/gardener.go:1870-1880`).
   - `gb trigger create schedule` — `--cron` defaults to `*/30 * * * *`
     (`defaultScheduleCron`, `trigger_schedule.go:28`; MR !4947), `--tz` UTC,
     `--cooldown 5m`, `--max-concurrent 3` (`trigger.go:109-110`).
2. **Per-trigger hand-authored values** — whatever the author passes on the
   flags, or writes by hand into a `kd create --type trigger` bead.

There is **no layer in between**. Every project inherits the same global
defaults, and any project-specific cadence has to be re-typed (and re-corrected)
on every gardener. The matt-gardenio build surfaced this directly: gardener
ka-0000003149 was hand-rolled, its cadence/role/cooldown were picked by hand,
and a too-slow cron had to be corrected after the fact. Different projects
legitimately want different baselines — e.g. gardenio wants a tighter loop than a
slow-moving infra repo, and a project pinned to `America/Los_Angeles` shouldn't
have to restate `--tz` on every trigger.

**Goal:** a per-project defaults layer that supplies baseline gardener /
schedule-trigger fields, with precedence **explicit flag > project default >
global binary default**, opt-in and non-breaking for everything that exists.

---

## 2. The precedent we build on

The `type=project` config bead already **is** the per-project configuration
layer for this platform. `ListProjectBeads` reads it into `ProjectInfo`
(`gardenio/controller/internal/beadsapi/projects.go`), and that struct already
carries a large cluster of per-project knobs that follow exactly the semantics
we want here:

| Field on project bead | `ProjectInfo` | Semantics |
|---|---|---|
| `default_timezone` | `DefaultTimezone` | scalar string default |
| `secrets` (JSON array) | `Secrets` | structured JSON list |
| `repos` (JSON array) | `Repos` | structured JSON list |
| `channel_roles` (JSON object) | `ChannelRoles` | structured JSON map |
| `execution_target` | `ExecutionTarget` | scalar, **precedence global < project < role < agent** |
| `ultracode` | `Ultracode *bool` | **tri-state** via `parseTriBool` (unset ≠ false) |
| `cpu_request` / `memory_limit` / … | resource overrides | scalar defaults |
| `schedules_enabled` | `SchedulesEnabled` | bool, `!= "false"` ⇒ **default true** |

Two patterns from this cluster are load-bearing for our design:

- **Tri-state via `parseTriBool`** (`roles.go:67`) — an *unset* layer is skipped
  when resolving, distinct from an explicit `false`. This is exactly how we
  distinguish "project didn't set a gardener default" from "project set it to a
  zero/empty value on purpose" (§5).
- **Layered precedence already exists** — `execution_target`, `ultracode`
  document `global < project < role < agent`. We slot gardener defaults into the
  same mental model.

`default_timezone` deserves special note: it is *already* a per-project gardener
default in everything but name — schedule triggers just don't consume it yet as
a cron-timezone fallback. Our design generalizes that one-off into a coherent
group.

---

## 3. Schema — the `gardener_defaults` field

**Storage decision (see §4): a single JSON object field named
`gardener_defaults` on the `type=project` bead.** Fields are all optional; an
absent field means "inherit the next layer down."

```text
// gardener_defaults on a type=project bead (pretty-printed; stored as compact JSON)
{
  "cron":            "*/20 * * * *",        // 5-field cron; validated with scheduler.ParseCron
  "timezone":        "America/Los_Angeles", // IANA tz; overrides/aliases default_timezone (§8)
  "cooldown_minutes": 15,                   // integer minutes; MUST be < cron period (advice ka-0000000174)
  "max_concurrent":  2,                     // int >= 1; agents fired per evaluation cycle
  "role":            "crew",                // default spawn role for gardeners in this project
  "allowed_roles":   ["crew", "triage"],    // roles a gardener here may spawn (superset gate)
  "allowed_projects":["gardenio"],           // projects a spawned agent may be scoped to
  "model":           "",                    // default model tier ("" = inherit global/role)
  "run_wallclock":   "45m",                 // per-run wall-clock ceiling (go duration)
  "loop_wallclock":  "8h"                   // whole-loop wall-clock ceiling (go duration)
}
```

### 3.1 Field table

| Field | Type | Validation | Applies to | Notes |
|---|---|---|---|---|
| `cron` | string | `scheduler.ParseCron` must accept; 5-field | gardener start, schedule trigger | Empty/absent ⇒ next layer (global deterministic-staggered / `*/30`). |
| `timezone` | string | valid IANA (`time.LoadLocation`) | schedule trigger `--tz` | If absent, fall back to existing `default_timezone` for continuity (§8). |
| `cooldown_minutes` | int | `>= 0`; **SHOULD be `< cron period`** | gardener start, schedule trigger | A *lint*, not a hard reject, so an author can knowingly widen it. Warn on ≥ period (advice ka-0000000174). |
| `max_concurrent` | int | `>= 1` | gardener start, schedule trigger | |
| `role` | string | non-empty; known role | gardener start (`--role`) | Default spawn role. |
| `allowed_roles` | []string | each a known role | gardener start | Gate on what the gardener may spawn. |
| `allowed_projects` | []string | each a known project | gardener start | Cross-project spawn gate. |
| `model` | string | known model id/tier or `""` | gardener/agent spawn | `""` = inherit; tri-state-ish (empty = unset). |
| `run_wallclock` | string | `time.ParseDuration` | run budget | Per-run ceiling. |
| `loop_wallclock` | string | `time.ParseDuration` | loop budget | Whole-loop ceiling. |
| `prompt_preamble` | string | free text, size-capped | gardener/agent prompt | **Deferred** — see §7 open question. Not in v1. |

### 3.2 Validation rules

- Validation happens **at write time** (CLI `gb project set-gardener-defaults`,
  §4.2) *and* defensively **at read time** in the controller. A malformed
  `gardener_defaults` JSON must **fail soft**: log a warning, treat the whole
  field as absent, and fall through to global defaults — never crash the
  TriggerEngine or block a gardener from firing. (Mirrors how a bad
  `channel_roles`/`secrets` JSON is tolerated today via `unmarshalProjectField`.)
- `cron` and `timezone` are validated with the same parsers the CLI already uses
  (`scheduler.ParseCron`, `time.LoadLocation`) so a project default can never
  encode a cron the engine would later reject.
- Unknown keys are **ignored** (forward-compat: a newer field on an older
  daemon degrades to "unset", not an error).

---

## 4. Storage — recommendation: **project-bead field, not a checked-in file**

The bead poses this as project-bead-field vs. checked-in config file vs. both.
**Recommendation: the project-bead `gardener_defaults` field. Do not add a
checked-in file.**

### 4.1 Why the project bead wins

| Criterion | Project-bead field (`gardener_defaults`) | Checked-in `.gardenio/gardener-defaults.yaml` |
|---|---|---|
| **Already-loaded** | Daemon loads `type=project` beads into `ProjectInfo` on the existing path — zero new I/O, zero new watcher. | Requires the controller to clone/checkout each project repo and read a file — a new dependency the controller does not currently have on the CRITICAL scheduling path. |
| **Consistency** | Sits beside `secrets[]`, `repos[]`, `default_timezone`, `execution_target` — one place operators already look. | Splits project config across two sources of truth. |
| **Multi-repo projects** | One project ↔ one bead, unambiguous. | Which repo's file wins? matt-gardenio has primary + reference repos; monorepo is one repo but many components. |
| **Cross-project defaults** | A project can default agents into `allowed_projects` it doesn't own the repo for. | A file in repo A can't naturally speak for project B. |
| **Editability** | `kd`/`gb` CLI + the P4 web config editor (`BeadID` retained for PATCH, projects.go §4 ka-0000008001) — no MR, no CI minutes (advice ka-0000001560). | Editing a checked-in YAML costs a commit + MR + CI pipeline every time cadence is tuned. |
| **Live reconfig** | Field edit reflected on next project-bead read; no redeploy. | File change needs a re-clone/re-read cycle. |
| **Timezone precedent** | Extends `default_timezone`, already a bead field. | Would orphan `default_timezone` on a different mechanism. |

The checked-in-file idea is attractive for "config-as-code lives with the repo,"
but the controller does not read project repos on the scheduling path, and
gardener cadence is exactly the kind of knob operators tune interactively and
often (the matt-gardenio correction). A field edit is cheaper and lower-latency
than a commit+MR+CI cycle, and it keeps a single source of truth. **If** a
future requirement demands repo-local config-as-code, a thin `gb project
sync-gardener-defaults --from-file .gardenio/gardener-defaults.yaml` importer can
push a YAML file *into* the bead field — the bead stays the authority the daemon
reads. That keeps the door open without forking the read path (noted in §7).

### 4.2 Authoring surface

- **Read/write CLI:** `gb project set-gardener-defaults <project> --cron … --cooldown … [--json <blob>]` writes the compact JSON into the field; `gb project show <project>` renders it. (Implementation-epic detail; not built here.)
- **Web:** the P4 config editor PATCHes the field on the retained `BeadID`.
- **Raw:** `kd update <project-bead> -f gardener_defaults='{…}'` for break-glass.

---

## 5. Precedence & override semantics

**Resolution order (highest wins):**

```
explicit CLI flag  >  gardener_defaults (project bead)  >  global binary default
```

For a spawned/hand-rolled trigger bead there is a fourth, lowest-priority
concept — the *global binary default* is what the CLI stamps when neither the
flag nor the project layer supplies a value.

### 5.1 Where the merge happens: **CLI at author time (v1), engine as guardrail (v2)**

There are two candidate merge points:

1. **CLI author-time resolution (recommended for v1).** `gb gardener start` /
   `gb trigger create schedule` resolve `flag → project → global` *before*
   writing the trigger/gardener bead, so the bead is written with **concrete,
   materialized values**. Pros: the stored bead is self-describing (what you see
   in `kd show` is what fires — no hidden layer), the TriggerEngine stays
   unchanged, and it composes with hand-authored beads (author runs the CLI).
   Cons: a project-default change does *not* retroactively re-cadence
   already-created gardeners (they froze their values at author time).
2. **Engine read-time resolution (v2, optional).** The TriggerEngine, when a
   trigger/gardener bead **omits** a field, looks up the owning project's
   `gardener_defaults` at fire time. Pros: change a project default and every
   gardener that didn't override picks it up live. Cons: `kd show` no longer
   tells you the effective cadence without also reading the project bead; the
   engine's `TriggerContext` (already carries `Project`, `DefaultTimezone` —
   `triggers.go:534-537`) grows a `GardenerDefaults` lookup.

**Recommendation:** ship **v1 = CLI author-time** first (smallest, safest, keeps
the hot scheduling path untouched), and treat engine read-time as a deliberate
v2 that is only worth it if "retune a project's cadence in one edit" becomes a
real operator need. The two are compatible: v1's materialized beads keep working
after v2 lands (an explicit value still beats the project layer).

### 5.2 Unset vs. explicit-empty — the tri-state rule

The subtle case the bead calls out: distinguishing **"project left this field
unset"** (⇒ inherit global) from **"project set it to empty/zero on purpose"**
(⇒ that empty value is the default).

Rules, mirroring `parseTriBool`/`ultracode` (unset ≠ false):

- **JSON key absent** ⇒ field is **unset** ⇒ inherit the next layer down.
- **JSON key present** (even with `""`, `0`, `[]`) ⇒ **explicit** ⇒ that value is
  the project default and it beats the global binary default.
- **CLI flag** distinguishes the two with cobra's `Flags().Changed(name)`: a flag
  the author actually typed always wins; a flag left at its zero value does
  **not** clobber a project-supplied default. (This is the standard "was the
  flag set?" guard — without it, a flag's own default silently overrides the
  project layer and the whole feature no-ops.)

Worked example — project sets `"model": ""` deliberately (meaning "force inherit
global, ignore any role default"):

```
gardener_defaults.model = ""   (present ⇒ explicit "inherit global")
role default              = "opus"
resolved model            = ""  → global model tier   (project's explicit-empty beat the role layer)
```

vs. project omits `model` entirely:

```
gardener_defaults has no "model" key   (absent ⇒ unset)
role default                          = "opus"
resolved model                        = "opus"          (fell through to the role layer)
```

---

## 6. Example configs

### 6.1 gardenio — tight loop, LA time (project bead ka-0000001738)

```json
{
  "cron": "*/20 * * * *",
  "timezone": "America/Los_Angeles",
  "cooldown_minutes": 15,
  "max_concurrent": 3,
  "role": "crew",
  "allowed_roles": ["crew", "triage", "gardener"]
}
```

Rationale: gardenio is the platform's own fast-moving repo; a 20-minute
supervision loop (cooldown 15 < 20, per advice ka-0000000174) keeps gardeners
responsive. LA timezone matches the existing `default_timezone`.

### 6.2 monorepo — slower cadence, business scope (project bead ka-0000000797)

```json
{
  "cron": "*/30 * * * *",
  "timezone": "America/Los_Angeles",
  "cooldown_minutes": 25,
  "max_concurrent": 2,
  "role": "crew"
}
```

Rationale: a large, slower-moving repo is well served by the standing `*/30`
norm; cooldown 25 stays under the 30-minute period.

### 6.3 devsecops — hourly, single-fire, review-oriented (project bead ka-0000004921)

```json
{
  "cron": "0 * * * *",
  "timezone": "America/Los_Angeles",
  "cooldown_minutes": 20,
  "max_concurrent": 1,
  "role": "triage",
  "allowed_roles": ["triage"]
}
```

Rationale: security/review work tolerates an hourly cadence and wants
single-fire (`max_concurrent: 1`) so passes don't stack. This is a case where the
project baseline *deviates* from the global `*/30` norm — exactly the reason the
per-project layer exists — and the deviation is one declared line, not a per-gardener
correction.

> Note: absent `gardener_defaults`, **all three** projects behave exactly as
> today (§7). These blocks are opt-in overrides, not required config.

---

## 7. Backwards compatibility & migration

- **Absent field = zero behavior change.** A project bead with no
  `gardener_defaults` key resolves to *exactly* today's global binary defaults.
  The read path is additive: `ProjectInfo` gains one nullable field; existing
  fields and every existing gardener are untouched.
- **Existing gardeners unaffected.** In the v1 (CLI author-time) model, already
  authored gardener/trigger beads carry materialized values and never consult the
  new layer — they keep firing identically. New gardeners opt in simply by being
  authored in a project that has `gardener_defaults` set.
- **Malformed field fails soft** (§3.2): a bad JSON blob logs a warning and is
  treated as absent, so a fat-fingered edit can never dark a project's gardeners.
- **Interaction with the global `*/30` norm (advice ka-0000000174).** The norm
  stays the *global* default. A project's `gardener_defaults.cron` is an
  intentional, declared deviation from it (devsecops hourly, gardenio 20m). The
  `cooldown < period` lint (§3.1) enforces the *spirit* of the norm — cooldown
  must not suppress the chosen cadence — regardless of what period the project
  picks. Nothing about this feature relaxes the norm for projects that don't set
  the field.
- **`default_timezone` continuity.** `gardener_defaults.timezone`, when absent,
  falls back to the existing `default_timezone` field so no project loses its
  current tz behavior. When both are present, `gardener_defaults.timezone` wins
  for cron evaluation (documented alias; a follow-up can deprecate the older
  field once every project has migrated). No silent divergence: the resolver
  reads one, then the other, deterministically.

---

## 8. Future extensibility

The `gardener_defaults` object is deliberately a single JSON blob so new keys are
purely additive (unknown keys ignored on older daemons — §3.2). Candidate
future project-scoped defaults:

- **`prompt_preamble`** — a per-project preamble prepended to gardener/agent
  prompts (deferred from v1; interacts with the pre-prompt work, bug
  ka-0000001234 / decision ka-0000003631 — hold until that lands so we don't
  build two competing preamble mechanisms).
- **`model` per-role sub-map** — `{"model_by_role": {"crew": "sonnet",
  "gardener": "opus"}}` once model-tiering matures.
- **`quiet_hours` / `active_hours`** — a project-wide business-hours window that
  auto-composes into cron (`*/30 9-17 * * 1-5`) without every author restating
  it.
- **`default_bead_query`** — a project-standard self-quiescing gate so idle
  gardeners are cheap by default.
- **`wall_clock_by_role`**, **`respawn_budget`** — richer run/loop budgets keyed
  by role (composes with the lineage respawn-budget enforcer already in the
  controller).
- **File-import bridge** — the optional `gb project sync-gardener-defaults
  --from-file .gardenio/gardener-defaults.yaml` importer (§4.1) if config-as-code
  becomes a hard requirement; the bead remains the authority the daemon reads.

Each of these follows the same precedence (`flag > project > global`) and
tri-state (`unset ≠ zero`) rules, so the resolver written for v1 generalizes to
all of them without redesign.

---

## 9. Open questions / alternatives considered

1. **Merge at CLI vs. engine (§5.1).** Recommended: CLI author-time for v1,
   engine read-time as an optional v2. *Open:* do operators actually need
   "retune once, re-cadence all" badly enough to pay the "kd show no longer
   shows effective cadence" cost? Defer until asked.
2. **Checked-in file vs. bead field (§4).** Recommended: bead field. *Alternative
   rejected:* checked-in YAML as the authority — adds a repo-read dependency to
   the controller's scheduling path and forks the source of truth. Kept as an
   optional import direction only.
3. **Timezone field naming (§7).** `gardener_defaults.timezone` vs. reusing
   `default_timezone`. Recommended: new key wins, old key is the fallback, with a
   later deprecation. *Open:* whether to migrate all project beads eagerly or
   lazily.
4. **`cooldown < period`: lint or hard reject (§3.1).** Recommended: warn, don't
   reject, so an author can knowingly widen. *Open:* revisit if warn-only lets
   too many self-suppressing gardeners slip through.
5. **Role/agent layers.** This spec stops at `global < project`. The broader
   `global < project < role < agent` chain (already real for `execution_target`
   / `ultracode`) is a natural extension but out of scope here — gardener
   defaults are project-and-flag today; a role layer is a follow-up if needed.

---

## 10. Follow-up implementation epic (scope, do NOT build here)

Proposed child tasks for the implementation epic:

1. **Schema + read path** — add `GardenerDefaults` to `ProjectInfo`, parse the
   `gardener_defaults` field with fail-soft JSON handling and the tri-state
   rules; unit-test unset vs. explicit-empty.
2. **CLI author-time resolution** — teach `gb gardener start` /
   `gb trigger create schedule` / `gb plan gardener` to resolve
   `flag(Changed) > project > global`, using `Flags().Changed` guards.
3. **Authoring surface** — `gb project set-gardener-defaults` + `gb project show`
   rendering + P4 web editor PATCH.
4. **Validation** — write-time + defensive read-time validation
   (`ParseCron`, `LoadLocation`, `ParseDuration`, `cooldown < period` lint).
5. **Docs + advice** — update `gardener-authoring-guide.md`,
   `gardener-loop-runbook.md`, and cross-link advice ka-0000000174.
6. **(v2, optional)** engine read-time resolution in `TriggerContext`.

---

*References: advice ka-0000000174 (global `*/30` norm), MR !4947 (`--cron`
default), gardener ka-0000003149 + project bead ka-0000000568 (matt-gardenio
precedent), `gardenio/controller/internal/beadsapi/projects.go` (`ProjectInfo`
field cluster), `gardenio/controller/internal/scheduler/triggers.go`
(`TriggerContext`), `gardenio/controller/cmd/gb/{gardener,trigger,trigger_schedule}.go`
(flag defaults), docs/gardener-loop-runbook.md, docs/gardener-authoring-guide.md.*
