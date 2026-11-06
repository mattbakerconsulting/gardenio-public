# Short-title groomer

The short-title groomer fills `short_title` on beads that lack one. It is a
scheduled Claude Code agent, not daemon code: no kbeads or controller code
calls a model. Plan: `gardenio/docs/superpowers/plans/2026-10-14-short-titles.md`
(decision D5, Task 9); epic gd-YZATAIOhfW.

| Piece | What it is |
|---|---|
| Role bead | `short-title-groomer`, `type=role`, `fields.model=anthropic/claude-haiku-4-5` (the fleet's Haiku LiteLLM route); no `ownership_grants` |
| Trigger bead | `type=trigger`, `trigger:schedule`, cron `*/30 * * * *` UTC, `action=spawn_agent`, `max_concurrent=1`, cooldown 10m |
| Pre-flight | one `bead_query` leaf with `short_title_missing: true` (below) |
| Prompt | [`short-title-groomer.prompt.md`](short-title-groomer.prompt.md), copied verbatim into the trigger's `prompt` field |

## The pre-flight gate

A `bead_query` condition accepts `short_title_missing` (bool). When true the
scheduler's count query (`conditions.evalBeadQueryCount`) passes
`ShortTitleMissing` to `ListBeadsQuery`, which the daemon serves as
`?short_title_missing=true`: only beads whose `short_title` is empty count.
The trigger fires only while that count is above zero, so a fleet where every
bead has a name spawns nothing.

```json
[{"type":"bead_query","op":">","value":"0","project":"gardenio",
  "labels":["project:gardenio"],
  "types":["task","bug","feature","epic"],
  "statuses":["open","in_progress","blocked"],
  "exclude_labels":["source:schedule"],
  "short_title_missing":true}]
```

`gb trigger create schedule --bead-query` takes it as the key
`short_title_missing=true` (any other value than a Go bool is an error).

Two deliberate differences from the plan's draft command, both so the gate can
reach zero and go quiet:

- **`exclude_labels=source:schedule`.** Schedule-run beads (`[HH:MM] Schedule: …`,
  label `source:schedule`) are named by the scheduler and the groomer skips
  them. Thousands of historical runs carry no `short_title`; counting them would
  keep the gate open forever and spawn a Haiku pod every 30 minutes to do
  nothing.
- **No `closed` in `statuses`.** The groomer only fills closed beads updated in
  the last 30 days, as the tail of a run. Counting every closed bead (about 1,500
  older ones) would likewise never reach zero. A bead closed before it was
  named gets filled by the next run that open work triggers.

Escalations are left out on purpose. On 2026-10-23 a run set `short_title` on
four escalations and the bridge woke an executor for each approved one within a
second, one on an owner-held merge (gd-TMYvBxNY6h). The bridge now wakes an
executor only on the approval event itself, but a scheduled pass has no reason
to write to the approval path at all, so keep `escalation` out of `types` here
and in the prompt.

## What one run does

1. Lists up to 50 open/in_progress/blocked beads missing `short_title`
   (task, bug, feature, epic; schedule runs excluded),
   then tops up with closed beads updated in the last 30 days, newest first.
2. For each, reads the title and the first 300 characters of the description,
   writes `kd update <id> --short-title "<text>" --only-if-short-title-empty`.
   The guard means an author who sets a name concurrently always wins; the
   groomer never edits `title`.
3. Rules: at most 40 characters, imperative or a plain noun phrase, no ids,
   timestamps, type words, bracket tags or `P0:`/`Task 9:` ordinals.
4. Squawks `short-title groomer: filled N, skipped M, remaining R` and
   `gb stop`s with the same counts in the wrap-up.

## Creating it (live, after the short-title releases roll)

```bash
kd create "short-title-groomer" --type=role -l project:gardenio \
  -f model=anthropic/claude-haiku-4-5 \
  --short-title "short-title groomer" \
  -d "Haiku-pinned groomer that writes short_title on beads missing one (plan 2026-10-14-short-titles)."

gb trigger create schedule \
  --title "Short-title groomer: fill missing short_title (50/run)" \
  --cron "*/30 * * * *" --tz UTC \
  --action spawn_agent --role short-title-groomer --agent-project gardenio \
  --allowed-role short-title-groomer --allowed-project gardenio \
  --max-concurrent 1 --cooldown 10m --enabled=false \
  --bead-query 'project=gardenio,types=task|bug|feature|epic,statuses=open|in_progress|blocked,exclude_labels=source:schedule,short_title_missing=true,op=>,value=0' \
  --prompt "$(cat gardenio/docs/groomers/short-title-groomer.prompt.md)"

kd update <trigger> -f wake_intent="Every 30 minutes, if any open task/bug/feature/epic lacks short_title, spawn one Haiku groomer that fills up to 50 of them (open first, then closed updated in the last 30 days) and exits."
```

The role needs no `ownership_grants` row. The daemon's ownership gate
(`authorizeOwnership`) refuses an update on a bead the caller did not create,
is not assigned and does not own, which is every bead the groomer names. The
first hand run (gd-TNonvjJY9j) wrote nothing for this reason. kbeads now has a
narrow exception instead (gd-JOOXA7UqLh,
`kbeads/internal/server/short_title_fill_authz.go`): any agent on its
per-agent token may set an EMPTY `short_title`, and nothing else, on a
non-decision bead that carries its token's project label. So the groomer can
only name `project:gardenio` beads, and never decisions. That is why
`decision` is left out of the pre-flight and the prompt: every in-repo creator
names decisions at create (`beadsapi.CreateBeadDetail`, Task 6), and a groomer
retrying refused decisions would spend its 50 slots on them every run.

Check with `kd show <trigger> --json`: `enabled` is JSON `false` and
`conditions[0].short_title_missing` is `true`. It stays disabled until the
backfill step (Task 10) runs `gb trigger enable <trigger>`.

Run it once by hand, without the cron:

```bash
gb spawn --role short-title-groomer --prompt "$(cat gardenio/docs/groomers/short-title-groomer.prompt.md)"
```

## Changing the prompt

Edit `short-title-groomer.prompt.md` in a PR, then copy it onto the live
trigger: `kd update <trigger> -f prompt=@gardenio/docs/groomers/short-title-groomer.prompt.md`.
The file and the trigger's `prompt` field should never differ.
