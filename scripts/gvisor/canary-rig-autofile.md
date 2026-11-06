# gVisor canary rig — R3 auto-file helper

`file-next-layer-bug.sh` (Rig **R3**, [ka-0000004124]) turns a gVisor
freeze→thaw **canary rung failure** into a tracked, deduped, linked
`type=bug` instead of a lost log line. It is the helper the bounded canary
driver **R4** ([ka-0000006231]) calls on the **first failing rung**, after **R2**
([ka-0000006948]) has captured the forensic bundle, and before HARD HOLD.

It is **harness-only**: it files/annotates beads. It never spawns a canary,
never touches prod, and never reads or flips the `freezeThaw` flag.

## Where it sits in the rig

| Rig | Bead | Role |
|---|---|---|
| R1 | [ka-0000005639] | Codifies the 7 named rungs + stable `FT_RUNG_*` markers. The **first failing rung** is the classified mode. |
| R2 | [ka-0000006948] | On first failing rung, snapshots forensics to a `gb bundle`. R3 consumes its bundle id/URL. |
| **R3** | **[ka-0000004124]** | **This.** Files (or dedup-comments) the next-layer bug with the failing rung, reason, and bundle link; wires `blocks`/`parent` edges. |
| R4 | [ka-0000006231] | Bounded self-loop driver. On any fail: R2 capture → **R3 file** → HARD HOLD (no same-layer auto-retry). |

## What the filed bug guarantees

Modeled on the exemplar next-layer bug [ka-0000000357] (precise symptom, root
cause hooks, fix directions, impact, links). Every filed bug:

- captures the **failing rung**, the **reason**, and the **R2 bundle link**
  (rendered with the mandatory `/beads/` path prefix — advice ka-0000003730);
- **blocks** the canary task **[ka-0000001355]** — the canary cannot be declared
  clean while the rung fails (`kd dep add ka-0000001355 <bug> --type blocks`);
- is a **child of the Phase-4 epic [ka-0000000867]**
  (`kd dep add <bug> ka-0000000867 --type parent-child`);
- carries the dedup labels `ft-sig:<hash>` and `ft-rung:<rung>` (plus
  `gvisor-canary`, `project:gardenio`, `needs-intake`).

## Dedup — (rung + reason signature)

The signature is `sha1(rung + basis)[:12]`, where the **basis** is the
classified `--mode` when given, else the **normalized** free-text reason.
Normalization strips volatile *value* tokens — timestamps, node names, IPs, byte
sizes, hex ids, counts — so the **same failure mode maps to the same signature
across cycles** even when forensic details differ.

> Keep volatile context (cycle index, node, instance) in the structured flags
> (`--cycle`, `--node`, `--canary`, `--mode`), **not** smuggled into `--reason`.
> The normalizer strips volatile *values* but deliberately does **not** rewrite
> prose, so two genuinely different root causes never collapse to one signature.
> Pass `--mode` when R1 classified the failure — it is the most stable key.

Before filing, R3 looks for a **live** bug carrying `ft-sig:<hash>`, **scoped to
the same project the filed bug is labelled with** (so the lookup is independent
of the calling agent's ambient `$BOAT_PROJECT`):

- **live match found** → it **comments** a recurrence note (with the fresh
  bundle) on that bug and re-asserts the two contract edges idempotently — no
  duplicate;
- **only a terminal match (`closed` *or* `partial`) / no match** → it **files a
  fresh bug**. Both `closed` and `partial` are terminal-for-progression in
  kbeads (a `partial` bug no longer blocks the canary), so a recurrence after
  either must produce a new, live blocker.

If the dedup query itself **fails** (daemon unreachable / auth blip), R3 **fails
loud** and files nothing — it never files blind, because the canary self-loop
re-drives the same failure and a blind file would spew one bug per cycle.

## Result lines

- `R3_RESULT=created bug=<id> sig=<sig>` — new bug filed, both edges wired.
- `R3_RESULT=deduped bug=<id> sig=<sig>` — commented on an existing live bug.
- `R3_RESULT=created_unlinked bug=<id> sig=<sig> blocks=<0|1> parent=<0|1>` —
  bug filed but a dep edge didn't land (transient). The `ft-sig` label is
  already on the bug, so the next dedup pass finds it and **repairs** the
  missing edges. R4 should surface this but must **not** re-file.

## Invocation (from R4, on first failing rung)

```sh
scripts/gvisor/file-next-layer-bug.sh \
    --rung "$FIRST_FAILING_RUNG" \
    --reason "$RUNG_REASON" \
    --bundle "$R2_BUNDLE_ID" \
    --mode "$CLASSIFIED_MODE" \      # optional: most stable dedup key
    --cycle "$CYCLE" --node "$NODE" --canary "$CANARY_BEAD" \
    --rungs-file "$FT_RUNG_DUMP"     # optional: embeds the full FT_RUNG_* set
```

Last stdout line is machine-readable for R4 to capture:

```
R3_RESULT=created bug=ka-XXXXXXXX sig=<sig>      # filed a new bug
R3_RESULT=deduped bug=ka-YYYYYYYY sig=<sig>      # commented on existing open bug
```

`--dry-run` prints the exact `kd` commands without mutating anything. The
`kd`/`gb` binaries are resolved via `$KD`/`$GB` so the hermetic test
(`file-next-layer-bug_test.sh`, wired into `make lint`) can shadow them — no
live daemon, no canary, no prod.

[ka-0000004124]: https://gardenio.e2e.example.com/beads/ka-0000004124
[ka-0000006231]: https://gardenio.e2e.example.com/beads/ka-0000006231
[ka-0000006948]: https://gardenio.e2e.example.com/beads/ka-0000006948
[ka-0000005639]: https://gardenio.e2e.example.com/beads/ka-0000005639
[ka-0000001355]: https://gardenio.e2e.example.com/beads/ka-0000001355
[ka-0000000867]: https://gardenio.e2e.example.com/beads/ka-0000000867
[ka-0000000357]: https://gardenio.e2e.example.com/beads/ka-0000000357
