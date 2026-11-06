# Formula Reference

Formulas define reusable, composable workflow templates that create bead hierarchies when instantiated.

## Quick Reference

```bash
kd formula cook <id> --var key=val     # preview expansion (no beads created)
kd formula cook <id> --var key=val --verbose  # include skipped steps
kd formula pour <id> --var key=val     # instantiate as persistent molecule (liquid phase)
kd formula wisp <id> --var key=val     # instantiate as ephemeral molecule (vapor phase)
kd formula apply <path> --var key=val  # apply from local JSON file
kd formula expand <id> --values a,b,c  # evaluate on_complete expansion
gb plan pour <plan> --formula <id> --var key=val  # pour INTO a plan as a phase
kd mol gc --age 24h                    # garbage-collect old wisps
kd mol gc --age 1h --dry-run           # preview what would be cleaned
kd mol bond <a> <b> --type conditional # link molecules with conditional activation
```

## Formula Types

Set `formula_type` in the formula JSON:

| Type | Description | Can pour/wisp? |
|------|-------------|----------------|
| `workflow` (default) | Sequential/parallel steps creating issue beads | Yes |
| `expansion` | Inline template for composition into other formulas | No |
| `aspect` | Before/after hooks that attach to matching steps | No |

## Phase

Formulas can declare a recommended instantiation phase:

- `"phase": "vapor"` — recommends `wisp` (ephemeral, for releases/health checks)
- `"phase": "liquid"` — recommends `pour` (persistent, for features/bug fixes)
- Omit if the formula works for both. A warning is printed if the wrong method is used.

## Variables

```json
{
  "vars": [
    {"name": "component", "required": true, "description": "Component to deploy"},
    {"name": "version", "required": true, "pattern": "^\\d{4}\\.\\d+\\.\\d+$"},
    {"name": "env", "default": "staging"}
  ]
}
```

- `required`: must be provided at pour/wisp time
- `default`: used when not provided
- `pattern`: regex validation at apply time (bad regex rejected at creation time)

## Steps

Each step becomes a bead when the formula is instantiated.

```json
{
  "steps": [
    {"id": "build", "title": "Build {{component}}", "short_title": "build {{component}}", "type": "task"},
    {"id": "test", "title": "Test {{component}}", "type": "task", "needs": ["build"]},
    {"id": "deploy", "title": "Deploy", "type": "task", "depends_on": ["test"]}
  ]
}
```

- `needs` is a readable alias for `depends_on` — both are merged and deduplicated
- `{{variable}}` substitution works in id, title, short_title, description, and depends_on refs
- `short_title` is the name humans see for the step bead (at most 40 characters). When a
  step omits it, the pour cuts one from the title at a word boundary, so every step
  satisfies `BEADS_SHORT_TITLE_GATE=enforce`. The molecule's is cut from its title.
- Every step bead gets `source_formula` and `source_step_id` fields for tracing

## Inheritance (extends)

```json
{
  "extends": ["ka-parentFormula"],
  "vars": [{"name": "extra_var"}],
  "steps": [
    {"id": "deploy", "title": "Custom deploy"},
    {"id": "extra-step", "title": "Child-only step", "depends_on": ["deploy"]}
  ]
}
```

Merge rules:
- **vars**: child overrides parent vars with same name; new vars appended
- **steps**: child overrides parent steps with same ID; new steps appended
- **default_roles, phase**: child overrides if set, else inherits
- Recursive resolution (max depth 10), circular inheritance detected and rejected

## Composition

### Expand (inline expansion)

Replace a step with the contents of an expansion-type formula:

```json
{"id": "deploy", "expand": "ka-deployExpansion", "expand_vars": {"environment": "production"}, "depends_on": ["build"]}
```

- Expansion steps get prefixed IDs: `deploy/pre-deploy`, `deploy/rollout`, etc.
- Internal deps within the expansion are prefixed automatically
- Root expansion steps inherit the expand step's `depends_on`
- `expand_vars` merge with parent vars (expand_vars override)
- Cannot combine with `loop`, `gate`, or `children`

### Children (hierarchical)

```json
{
  "id": "backend", "title": "Backend", "type": "epic",
  "children": [
    {"id": "api", "title": "Build API", "type": "task"},
    {"id": "db", "title": "Schema migration", "type": "task", "needs": ["api"]}
  ]
}
```

- Parent becomes an epic, children become tasks linked via parent-child dependency
- Step IDs must be globally unique across all nesting levels
- Cross-level deps work

### Loop (iterative expansion)

Three modes:

```json
{"loop": {"var": "region", "values": ["us-east-1", "eu-west-1"]}}
{"loop": {"var": "i", "count": 3}}
{"loop": {"var": "n", "range": "1..5"}}
```

- Loop var substituted in step ID, title, short_title, description, and depends_on
- Exactly one mode per loop; range is inclusive

### Branch (conditional)

```json
{
  "id": "deploy-step",
  "branch": {
    "on": "{{env}}",
    "cases": {
      "prod": {"expand": "ka-0000002326", "expand_vars": {"region": "us-east-1"}},
      "staging": {"expand": "ka-stagingDeploy"}
    },
    "default": {"expand": "ka-devDeploy"}
  }
}
```

- Resolves during Expand phase; matched case expansion must be `formula_type=expansion`
- Cannot combine with `expand`, `loop`, `gate`, or `children`

### Gate (decision/sync/manual)

```json
{"id": "approve", "gate": {"type": "decision", "prompt": "Deploy to prod?", "options": [{"id": "yes", "label": "Deploy"}, {"id": "no", "label": "Skip"}], "timeout": "24h"}}
{"id": "sync-point", "gate": {"type": "sync"}}
{"id": "manual-step", "gate": {"type": "manual", "timeout": "24h"}}
```

| Type | Behavior |
|------|----------|
| `decision` | Creates decision-kind bead, integrates with `gb decision` and Slack UI |
| `sync` | Blocks until all `depends_on` steps complete (fan-in barrier) |
| `manual` | Requires external trigger (human action, webhook) |

A `decision` gate step pours as a `type=decision` bead; a `manual` or `sync` gate step keeps
`type: task` (the daemon's task schema declares `gate_type` and `timeout`). `kd formula cook`
prints each gate after its step, e.g. `{gate: manual, timeout 24h}`, and `--json` carries it
as `gate`. A gate's `prompt` and option labels are not `{{var}}`-substituted, so keep vars out
of them. Worked examples: `gardenio/formulas/onboarding/`.

### Aspect (cross-cutting)

Aspect formulas inject before/after steps around matching workflow steps:

```json
{
  "formula_type": "aspect",
  "match": "deploy-*",
  "before": [{"id": "pre-check", "title": "Pre-deploy check"}],
  "after": [{"id": "post-verify", "title": "Post-deploy verify"}]
}
```

- Glob patterns: `deploy-*` (prefix), `*-check` (suffix), `*deploy*` (contains), `*` (all), exact
- Injected steps get prefixed IDs: `step-id/before/aspect-step-id`
- Before steps inherit matched step dependencies; after steps depend on matched step
- Referenced via `"aspects": ["ka-aspectId"]` in the workflow formula

### on_complete (deferred expansion)

Triggers expansion when a step is closed, based on step output:

```json
{
  "on_complete": {
    "for_each": "{{scan.output.components}}",
    "var": "component",
    "expand": "ka-componentProcess",
    "expand_vars": {"name": "{{component}}"}
  }
}
```

- Stored in step bead fields at pour time, evaluated at step close
- Expand with: `kd formula expand <step-bead> --values auth,api,web`
- Mutually exclusive with: `expand`, `branch`, `loop`, `gate`

## Skill Routing and Synchronization

### requires_skills

```json
{"id": "go-work", "requires_skills": ["go", "kubernetes"]}
```

- Skills become `skill:<name>` labels on step beads at pour time
- Can be set at formula level (inherited by all steps) or step level (overrides)

### waits_for

```json
{"id": "integration", "waits_for": "all-children", "children": [...]}
```

- Parent cannot close until all children complete
- Only `"all-children"` is currently valid

## Molecules

### Deduplication

`kd formula apply/pour` checks for existing open/in_progress molecules from the same formula with identical `applied_vars` and fails on duplicates. Use `--force` to override.

### Emplacing a pour into a plan (a poured molecule as a plan phase)

A pour can target a **`/plans` plan** so that the resulting molecule renders as a
first-class plan **phase** — the molecule is the phase, and its step children are
the phase's steps. This is the reverse entry into a plan: instead of hand-authoring
a phase step by step, pour a formula that already captures the work.

```bash
gb plan pour <plan-epic-id> --formula <formula-id> --var key=val [--force] [--ephemeral]
```

Equivalently, the daemon endpoint `POST /v1/formulas/{id}/pour` accepts a
`target_plan` field (blank = a freestanding molecule; the beads-viewer pour form
exposes it as the **Emplace into plan** input). When a plan is given the daemon,
in one request:

1. instantiates the formula (molecule + step children), then
2. parents the molecule under the plan epic (parent-child), and
3. appends the molecule id to the plan's `phase_order`.

The container stays **`type=molecule`** — it keeps its `/molecule` execution view,
`kd mol` verbs, wisp GC, the dedup guard above, and formula lineage. The plan
cockpit's phase predicate (`isPhaseContainer`) accepts a molecule wherever it
accepts a phase sub-epic, so the poured phase renders, counts toward progress, and
reorders exactly like a hand-authored phase. The target must be a real plan (a
`type=epic` bead carrying the `plan` label) or the pour is rejected before any bead
is created. (Epic ka-0000000759; decision ka-0000003759.)

### Conditional Bonds

```bash
kd mol bond ka-abc ka-def --type conditional
```

| Bond Type | Behavior |
|-----------|----------|
| `sequential` (default) | B runs after A completes |
| `parallel` | B runs alongside A |
| `conditional` | B activates only if A fails |

### Garbage Collection

```bash
kd mol gc --age 24h           # burn closed wisps older than 24h
kd mol gc --age 1h --all      # include open/in_progress wisps
kd mol gc --age 7d --dry-run  # preview what would be cleaned
```

Duration units: `s`, `m`, `h`, `d`. By default only closed wisps are collected.

## Filesystem Formulas

Pass a file path (containing `/` or `\`, ending in `.json`, or starting with `.`) to load from disk:

```bash
kd formula cook ./formulas/deploy.json --var env=prod
kd formula pour /tmp/my-formula.json --var component=auth
```

File-based formulas support all features. Formula ID recorded as `"file:path"`.
