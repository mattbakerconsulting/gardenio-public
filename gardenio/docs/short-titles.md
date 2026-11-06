# Short titles (`short_title`)

Every task, bug, feature, epic, decision and escalation bead carries a
`short_title`. It is the name of at most 40 characters that people see for the
bead. Slack prose and the References footer use it as the link text, and so do
beads-viewer list rows and agent cards. The `gd-*` id appears only in the link
URL. The long `title` stays as it is, and nothing ever rewrites it.

Plan: [`superpowers/plans/2026-10-14-short-titles.md`](superpowers/plans/2026-10-14-short-titles.md)
(epic gd-YZATAIOhfW, seed gd-V5nGseRS5f).

## The help text

The `kd create --short-title` / `kd update --short-title` flag help, the
daemon's warning and refusal, and the agent advice line all quote one constant,
`model.ShortTitleHelp` (`kbeads/internal/model/validate.go`):

```text
short_title is the name humans see for this bead: in Slack prose and the References footer, in beads-viewer lists and on agent cards. Up to 40 characters, imperative, no ids, timestamps, type words or prefixes. Rendered example: 🌱 forward duration_ms via gb hook relay (the glyph is added at render time; store only the words).
```

The example is `model.ShortTitleExample`, and a test checks that it passes the
validator. The plan's first draft used "forward duration_ms through gb hook
relay", which is 41 runes and so too long for its own limit.
`kbeads/internal/model/validate_test.go` also checks that this block matches
`ShortTitleHelp`, so update the two together.

## The field

- A nullable `beads.short_title` column (migration 056). An empty value means
  unset and is left out of the JSON (`omitempty`).
- The daemon trims the value and collapses runs of spaces before validating it.
  The limit is **40 runes** (characters, not bytes), and line breaks are
  rejected.
- Internal types (agent, session, message, bundle, trigger, …) are never gated.

Authoring:

```bash
kd create "P0: forward duration_ms + tool_use_id through gb hook relay" --type=task \
  -l project:gardenio --short-title "forward duration_ms via gb hook relay"
kd update gd-XXXX --short-title "forward duration_ms via gb hook relay"
kd update gd-XXXX --short-title "…" --only-if-short-title-empty   # never overwrite an authored value
```

`kd show` prints a `Short title:` line, and `kd list` has a `SHORT` column.
`kd list --missing-short-title` lists the beads that still lack one (HTTP
transport only).

Rules for a good one: imperative, the words a human would use for the work,
with no bead ids, timestamps, type words (`bug:`, `task`), bracket tags or
`P0:` ordinals. Slack shows it with the type glyph added, for example
`🌱 forward duration_ms via gb hook relay`, so store only the words.

An epic with **arms** (three to five emoji in a row,
[`heraldry/README.md`](heraldry/README.md)) shows its run in place of the type glyph in prose and
before the short title in card headers, viewer rows and `gb` lines:
`🔥🧭📦 forward duration_ms via gb hook relay`. The run counts as one glyph glued to the short
title; it is never part of the stored `short_title`.

The agent preamble (`gardenio/controller/internal/bridge/init_prompts.go`, the
`config:claude-instructions:*` beads that are upserted on bridge start) shows
`--short-title` and this example on the Essential Commands `kd create` line,
and every `kd create` example in it passes the flag. Tests in
`init_prompts_test.go` check both, so a new example without the flag fails CI.

## The gate: `BEADS_SHORT_TITLE_GATE`

The daemon checks every create of a gated type that has no `short_title`, in
`createBead`, right after the authoring-time human-operator gate
(`kbeads/internal/server/short_title_gate.go`). The mode is set by an env var:

| Value | Effect |
|---|---|
| unset, `warn` | **Default.** The create succeeds. An HTTP caller gets an `X-Beads-Warning` header. A gRPC caller gets only a daemon `slog.Warn`. |
| `enforce` (or `1`/`true`/`yes`/`on`) | A **non-human** caller is refused with a 400. Non-human means static, fleet or machine credentials: agent pods, bridges and services. A human caller (oidc or edge: kd on a laptop, the viewer) still only gets the warning, because the groomer fills in what a human skips. |
| `off`, or any value the daemon does not recognise | Disabled: no metric, no warning, no refusal. This is the kill switch. A typo turns the gate off rather than on. |

The Helm chart sets it with `beads.shortTitleGate`. When the value is empty,
the env is left unset and the daemon default `warn` applies. Changing the value
is a privileged deploy-values change: the flip to `enforce` happens only after
the audit below (plan Task 10).

### The warning and the refusal

Under `warn`, the HTTP create returns a header with one value per warning:

```text
X-Beads-Warning: short_title missing: <model.ShortTitleHelp>
```

`kd` prints each value to stderr as `warning: short_title missing: …`, and the
create still succeeds. Under `enforce`, a non-human caller gets a 400 error
whose body is:

```text
short_title is required for type task: <model.ShortTitleHelp>
```

Both texts contain the rendered example, so the fix is in the message itself.

## Fills nobody has to author

- **In-repo creators.** Every controller, gb, viewer and bridge create goes
  through `beadsapi.CreateBeadDetail`. When a gated type arrives with an empty
  `ShortTitle`, that function fills it in using `shorttitle.Derive(type, title)`
  (`gardenio/controller/internal/shorttitle`). Some shapes get a fixed value:
  schedule runs get `<trigger title> HH:MM`, agent beads get the handle, and
  plan epics get `plan: <goal>`. This keeps the enforce flip from blocking any
  in-repo creator (plan Task 6, gd-QypEmywEjJ; still in progress when this doc was written).
- **Rendering.** The bridge's `beadDisplayName` uses `short_title` first, then
  the older `fields.short_title`, then `shorttitle.Derive`, and finally the id.
  Every value is clipped at a word boundary, with a trailing `…` only when
  something was cut.

## The groomer

No daemon code calls a model. A scheduled Claude Code agent writes the short
titles that are missing (plan Task 9, gd-F3n1hBqhCt):

- a `type=role` bead `short-title-groomer`, pinned to
  `anthropic/claude-haiku-4-5`;
- a `type=trigger` / `trigger:schedule` bead that runs every 30 minutes. A
  `bead_query` with `short_title_missing=true` is checked first, so a fleet with
  nothing to fill costs nothing;
- a prompt kept in `gardenio/docs/groomers/short-title-groomer.prompt.md`. Each
  run fills up to 50 beads, open ones first and then beads closed in the last
  30 days. It writes with `kd update --short-title … --only-if-short-title-empty`,
  so a value someone wrote at the same time always wins. It never edits `title`
  and skips schedule-run and agent beads.

The trigger is created disabled and is enabled in plan Task 10, together with
the backfill.

## Auditing with the metric

Each create of a gated type adds one to

```text
gardenio_beads_short_title_gate_total{bead_type, caller, action}
```

with `caller` set to `human` or `agent`, and `action` set to `supplied`,
`allowed_warn` or `refused`. The query below shows who is still leaving the
field out:

```promql
sum by (caller, action, bead_type) (increase(gardenio_beads_short_title_gate_total[7d]))
```

The flip to `enforce` is safe once `caller="agent", action="allowed_warn"` has
been zero for the observation window. To find which creator is leaving it out,
look at the daemon's `short_title missing` warning logs, which carry `type`,
`actor` and `mode`. After the flip, any non-zero `action="refused"` is an agent
or service that still leaves the field out: fix the creator rather than turning
the gate off.
