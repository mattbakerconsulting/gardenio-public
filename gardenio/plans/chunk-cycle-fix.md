# chunk-cycle-fix — stop tree-hug chunk beads from cycling through ready

Bug: ka-0000002094. Tree-hug chunk beads under epic ka-0000004697 (e.g.
ka-0000000962, ka-0000005104, ...) cycle perpetually through the
gardener-intake ready queue:

1. gardener-intake claims them → `in_progress`, comments "Removing from
   ready queue; reopen if MR closes without merge."
2. gardener-intake despawns (ephemeral).
3. reviver (§0d) / reaper (§0d) sees dead assignee >30m, `kd unclaim` →
   `open` again.
4. next gardener-intake cycle re-picks them. Goto 1.

Net: ~7 beads permanently inflate the ready queue and absorb cycle
budget. Tree-hug schedule ka-0000005593 is *closed*, so no gardener is
asserting ownership of these chunks anymore — they are orphans whose
MRs are still genuinely waiting on human chunk_owners (matthewbaker,
gardeniobot, etc.).

## Recommendation: **Option A** — prompt-level query exclusion in
gardener-intake (and mirrored skip in reviver / reaper rescue)

Pick A over B and C because:

- **B (reviver no-reap on chunk_owner=human)** changes the meaning of
  "dead-agent >30m" rescue, the single load-bearing primitive that
  pulls every legitimate stale bead back into circulation. Threading
  a fuzzy "is `chunk_owner` a human name?" check into that hot path
  risks paper-cuts on the next agent who legitimately needs reaping
  (any agent whose name doesn't match the heuristic) and re-adds
  complexity every reviver/reaper cycle. The bug is upstream — intake
  should not have claimed the chunk in the first place.
- **C (tree-hug gardener stable assignee)** depends on schedule
  ka-0000005593 being open and actively running. It is currently
  closed; reviving it just to suppress this cycle is a much larger
  policy change than the bug warrants, and even when it ran it
  produced the very chunk beads now in the loop. It also pushes
  the fix into a per-schedule prompt living in the DB, not in source.
- **A** kills the loop at its source (gardener-intake's walk) with one
  guard clause, and the same clause drops into reviver/reaper §0d
  pre-check so neither gardener double-handles a parked-by-intent bead
  in the meantime. No code change to `gb ready` itself (which is
  shared infra across many callers).

Stable signal used by the guard: the chunk bead's description carries
a YAML frontmatter line `mr_state: open` (gitlab-bridge-maintained,
see e.g. `kd show ka-0000000962`). When that line is present the bead
belongs to the MR-owning lane (delivery / chunk_owner) and not to
intake. As a defense-in-depth secondary signal, the bead's transitive
parent epic carries `label=kind:tree-hug` (ka-0000004697); either
signal alone is sufficient.

## Patch shape (~5 lines, prompt-level)

**1. `gardenio/plans/gardener-prompts/intake.md` — §3 walk loop.** Add a
hard skip immediately inside the `for id in open_unscoped` loop, before
any scoping work:

```bash
# Skip beads whose description carries `mr_state: open` frontmatter:
# they belong to the delivery / tree-hug lane (chunk_owner owns the
# MR). Claiming them here triggers the reap-respawn cycle (bug
# ka-0000002094).
if kd show "$id" --json | jq -r '.description // ""' \
     | head -20 | grep -qE '^mr_state:[[:space:]]*open$'; then
  continue
fi
```

**2. `gardenio/plans/gardener-prompts/reviver.md` §0d and
`gardenio/plans/gardener-prompts/reaper.md` §0d — rescue loop.** Add
the same skip immediately after the existing "skip if already rescued
in last 30m" check, before `kd unclaim`:

```bash
# Parked-by-intent: a bead with open MR frontmatter was claimed by
# intake as a parking action. Do NOT rescue it — that re-opens the
# cycle in bug ka-0000002094.
if kd show "$id" --json | jq -r '.description // ""' \
     | head -20 | grep -qE '^mr_state:[[:space:]]*open$'; then
  continue
fi
```

Both prompts are source-controlled here; the runtime gardener reads its
prompt from its schedule bead, so each change also needs the operator
ritual `kd update <schedule-id> -f prompt="$(cat <file>)"`. Schedule
IDs: gardener-intake = ka-0000001610, reviver = ka-0000002321, reaper =
ka-0000001548.

## Risks / blast radius

- **Scope.** Three prompt files. No Go code change. No schema change.
  No daemon change. No `gb ready` behavior change for non-gardener
  callers.
- **False positive shape.** Any non-chunk bead whose description
  happens to contain a top-level `mr_state: open` line is also
  skipped by intake and not rescued. This is the intended behaviour:
  such a bead, by construction, has an open MR and should be handled
  by delivery, not intake. The `head -20` cap limits the match to
  the frontmatter region and avoids matching the substring inside a
  longer comment quote.
- **False negative shape.** A chunk whose MR closes (`mr_state:
  closed|merged`) is no longer skipped — intake and rescue resume
  normal handling, which is correct (the bead is now either done or
  needs follow-up work).
- **Gardener-intake KPI.** The seven cycling chunk beads stop counting
  toward `ready_queue_size` *and* stop being re-claimed each cycle.
  Net delta: intake KPI improves; tree-hug epic remains exactly where
  it is (waiting on human review of open MRs, which is the correct
  state).
- **Race with revived tree-hug gardener.** If schedule ka-0000005593
  is later reopened and the tree-hug gardener claims a chunk with a
  stable assignee, the guard is a no-op for the rescue path (the bead
  is no longer "stale dead-assignee" because the gardener is live) and
  still benign for the intake walk (intake also skips assigned beads).

## Test approach

1. **Replay on production data, dry-run.** For each of the seven
   cycling beads (ka-0000000962, ka-0000005104, ka-0000003444,
   ka-0000003496, ka-0000005423, ka-0000001653, ka-0000004637), run
   the proposed `kd show ... | grep` snippet. Expect: 7/7 match
   `mr_state: open`.
2. **Negative-control sweep.** Run the same snippet over the current
   `gb ready --all-projects --json | jq -r '.[].id'` set. Beads
   that match are the candidates the new guard would skip; manually
   verify each is in fact MR-parked (open MR, human chunk_owner).
   Expect: no genuine intake-scoping work matches.
   (`gb ready`/`kd list`/`kd search --json` emit a **bare array** —
   iterate with `jq '.[]'`, never a `.beads`-keyed form. See advice
   `ka-0000003286`.)
3. **Stage the prompt changes**: update intake first, observe two
   gardener-intake cycles, confirm zero "Removing from ready queue"
   comments land on the seven chunks. Then update reviver + reaper,
   observe two of each, confirm zero `reviver-rescued: previous
   assignee was dead >30m` / `reaper-rescued:` comments land on the
   seven chunks.
4. **Steady-state check (24h).** `kd show ka-0000000962 --json | jq
   '[.comments[] | select(.created_at > (now - 86400 | todate))] |
   length'` should drop from its current ~3/24h to 0/24h.
5. **Watch the intake KPI.** `gb ready --all-projects --json | jq
   '.beads | length'` baseline vs. one cycle after — expect a drop
   of ≥7 with no other queue changes.

## Out of scope (follow-ups, file separately if pursued)

- Promoting `mr_state` / `chunk_owner` from YAML frontmatter to
  first-class `kd` fields (so the daemon can index/filter them
  natively). This would let `gb ready` itself add a `-f
  mr_state!=open` filter and remove the per-bead `kd show` cost in
  the prompts. Worth doing if the same frontmatter pattern starts
  showing up in other intake bypass cases.
- Reopening + revising tree-hug schedule ka-0000005593 with the
  Option-C stable-assignee pattern. Independent of this fix.
- Adding a daemon-level "transitive parent label" filter so callers
  can say "exclude beads under any open `kind:tree-hug` epic". Useful
  if more lanes adopt the label.
