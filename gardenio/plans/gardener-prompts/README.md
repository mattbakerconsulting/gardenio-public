# Gardener schedule-bead prompts (source of truth)

The gardenio gardeners run from schedule beads whose `prompt` field
drives each cycle. Editing the prompt in the bead directly makes the
change invisible to review — no diff, no history, no MR. These files are
the version-controlled source of truth; the bead prompts are synced from
them.

> **Enforcement (bug `ka-0000000874`).** The repo→bead sync below is manual,
> and for a long time *nothing* enforced it — a merged `.md` edit could silently
> fail to reach the running gardener, and drift accumulated (8/9 gardeners drifted,
> 2026-08-05). Two mechanisms now guard against silent drift:
>
> 1. **Drift alarm** (dispatch mechanism dead, logic intact) — the scheduled
>    GitLab pipeline job `gardener-prompt-drift-alarm` used to dispatch
>    `.rwx/gardener-prompt-drift.yml`, an RWX workflow that read each mapped
>    gardener's *live* prompt from the durable prod daemon and diffed it against
>    its `.md` here (`scripts/check-gardener-prompt-drift.py`, driven by the
>    machine-readable [`gardeners.manifest.json`](gardeners.manifest.json)). The
>    2026-09-16 RWX decommission deleted `.rwx/` (and the rwx.com
>    account/cluster it dispatched to) wholesale, so that scheduled job no
>    longer has anything to dispatch — the alarm does not currently run. The
>    diff script and its unit test (`scripts/check-gardener-prompt-drift_test.py`)
>    still exist and still pass; only the CI dispatch that invoked them on a
>    schedule is gone pending a non-RWX replacement. It ran on the
>    **scheduled** pipeline, not per-MR: the per-MR CI pipeline holds only
>    `GITLAB_CLONE_TOKEN` and cannot reach the daemon at all, so a merge-time
>    gate was not possible without adding daemon creds to the untrusted-branch
>    pipeline.
> 2. **Pointer prompts** (rolling out, operator decision `ka-0000003376` chose
>    this option 2026-08-24) — the close-the-loop fix: flip each live `prompt` to a
>    thin stub that tells the gardener to read + follow its `.md` from the
>    freshly-cloned working tree at cycle start. That deletes the sync step
>    entirely, so the drift class cannot recur. Rewriting live prod gardener prompts
>    is a prod-blast change, so it is gated on that decision + a **gardener-by-gardener**
>    rollout (verify one live cycle before flipping the next); the drift alarm above
>    is pointer-aware (a recognized stub counts as in sync). The **pointer-stub
>    contract** is documented in [Syncing → Pointer stubs](#pointer-stubs-the-enforcement)
>    below. First gardener flipped: `oss-sanitization` (`ka-0000007535`), 2026-08-25.

> ⚠ **The `Schedule bead` ids below drift.** Gardeners are periodically re-authored
> under new `vN` ids, so a pinned id here goes stale/closed while the live gardener runs
> under a new id (as of 2026-08-25, 7 of these 9 were closed). Treat this column as a
> historical anchor, not a lookup key — **resolve the live bead by role** with
> `gb trigger list` (status=open, enabled=true) before any `kd update`/flip.

Only **live** gardeners are tracked by the drift alarm (`gardeners.manifest.json`);
the `.md` files below whose live bead has retired are kept for their history and
are listed in **Retired predecessors** — they are intentionally *out* of alarm scope
until a live successor is version-controlled (a row pinning a closed bead only emits a
phantom verdict — see the manifest `_comment` and design `ka-0000003223`).

| Gardener | Schedule bead (may be stale — resolve by role) | File | Alarm |
|---|---|---|---|
| bugfix | `ka-0000001007` | [`bugfix.md`](bugfix.md) | ✅ tracked |
| oss-sanitization | `ka-0000007535` | [`oss-sanitization.md`](oss-sanitization.md) | ✅ tracked (pointer stub) |
| investigative-janitor | `ka-0000007481` | [`investigative-janitor.md`](investigative-janitor.md) | ✅ tracked |
| intake-groomer | `ka-0000003841` | runbook [`docs/runbooks/intake-groomer.md`](../../docs/runbooks/intake-groomer.md) | ✅ tracked (runbook-delegating) |
| placement | _pending_ (see [`gardenio/plans/gardener-placement.md`](../gardener-placement.md)) | [`placement.md`](placement.md) | ⏳ not armed |
| canonical-prod-graduation | _pending arm_ (operator decision per advice `ka-0000003852`; anchor program `ka-0000000398`) | [`canonical-prod-graduation.md`](canonical-prod-graduation.md) | ⏳ not armed |
| delivery | live lane is **gardener-delivery-v2** `ka-0000007986` (inline prompt) — see Retired | [`delivery.md`](delivery.md) | ⛔ retired (see below) |
| release | live lane is **gardener-release-owner v4** `ka-0000003719` (runbook doc `ka-0000000327`) — see Retired | [`release.md`](release.md) | ⛔ retired (see below) |
| customer-poc-aws | no live successor — see Retired | [`customer-poc-aws.md`](customer-poc-aws.md) | ⛔ retired (see below) |
| reaper | no live successor — see Retired | [`reaper.md`](reaper.md) | ⛔ retired (see below) |
| reviver | no live successor — see Retired | [`reviver.md`](reviver.md) | ⛔ retired (see below) |
| wake-intent-reconciler | `ka-0000002948` (closed+disabled — see Retired) | [`wake-intent-reconciler.md`](wake-intent-reconciler.md) | ⛔ retired (see below) |

> **Retired predecessors** — when a schedule bead is replaced OR closes without a
> version-controlled successor, note it here and REMOVE its `gardeners.manifest.json`
> row so the mapping above stays unambiguous, stale bead ids don't resurface, and the
> drift alarm never byte-diffs a dead bead (a phantom verdict — ka-0000003148):
>
> | Gardener | Retired bead | Retired | Successor | Why re-authored (not re-enabled) |
> |---|---|---|---|---|
> | oss-sanitization | `ka-0000002056` | 2026-07-30 | `ka-0000007535` | Pre-epoch `CreatedAt` (2026-06-05) pins the `wall_clock` baseline to the 2026-07-03 grandfather epoch, so every legal `hard_stop wall_clock` ≤ the 720h guard ceiling is already met (or expires 2026-08-02, before the next Monday cron) — the pre-fire stop gate would reap it within one reconcile of re-enabling. Re-authored as a fresh bead for an honest 720h window; predecessor set `enabled=false` and kept `closed` for the audit trail. Plan `ka-0000005359`, decision `ka-0000004719`. |
> | intake | `ka-0000001610` | 2026-05-25 | `ka-0000003841` (groomer) | The live intake lane is the **gardener-intake groomer** (`ka-0000003841`; chain `ka-0000001610` → `ka-0000006210` → `ka-0000003841`). It reads its runbook from [`docs/runbooks/intake-groomer.md`](../../docs/runbooks/intake-groomer.md) on a fresh-`main` boot, **not** the `gardener-prompts/<name>.md` inline-copy pattern. As of `ka-0000002202` it is under **positive drift coverage** via a runbook-DELEGATING `gardeners.manifest.json` row (`name: intake-groomer`, `runbook: gardenio/docs/runbooks/intake-groomer.md`): the alarm asserts the runbook file exists **and** the live prompt still references it, rather than diffing an inline body — so a silently-broken delegation now reds instead of going unmonitored. The old inline `intake.md` is retained only as a redirect stub and MUST NOT be re-added as an inline-copy row. Design `ka-0000003223` §3, bug `ka-0000000874`. |
> | delivery | `ka-0000000377` | 2026-08-21 | `ka-0000007986` (v2) | Orig lane hard-stopped on `consecutive_failures` and was re-armed as **gardener-delivery-v2** (`ka-0000007986`, enabled, cron `7,27,47`; bug `ka-0000004120`, decision `ka-0000003365`). v2 carries an **inline** prompt and does NOT read `gardener-prompts/delivery.md`, so `delivery.md` monitors no live gardener. Manifest row removed to stop the phantom DRIFT (`ka-0000000874`). Re-add a tracked row only if v2 is version-controlled as a pointer/runbook lane. |
> | release | `ka-0000003737` | 2026-08 | `ka-0000003719` (release-owner v4) | The live release lane is **gardener-release-owner v4** (`ka-0000003719`, cron `13,43`), which delegates its per-cycle Steps 0–8 to doc `ka-0000000327` — a different identity from the retired `gardener-release` in `release.md` (`:07/:37`, two-part KPI). No live trigger reads `release.md`. Manifest row removed to stop the phantom DRIFT. Version-controlling release-owner + a manifest row is task `ka-0000001820` / operator decision `ka-0000000800` (prod-blast; doc `ka-0000001072`). |
> | customer-poc-aws | `ka-0000000154` | 2026-05-25 | _none_ | POC umbrella gardener (`ka-0000005972`), closed; no live successor reads `customer-poc-aws.md`. Manifest row removed to stop the phantom DRIFT (`ka-0000000874`). |
> | reaper | `ka-0000001548` | 2026-05-24 | _none_ | Closed+disabled; no live enabled successor reads `reaper.md`. Manifest row removed to stop the phantom DRIFT (`ka-0000000874`). |
> | reviver | `ka-0000002321` | 2026-05-24 | _none_ | Closed+disabled; no live successor reads `reviver.md` (`ka-0000006342` trigger-reviver is a different gardener). Manifest row removed to stop the phantom DRIFT (`ka-0000000874`). |
> | wake-intent-reconciler | `ka-0000002948` | 2026-08-21 | _none live_ | Closed+disabled (enabling was always an operator decision); no live gardener reads `wake-intent-reconciler.md`. Manifest row removed (was coincidentally byte-in-sync but pinned a closed bead — the next `.md` edit would have flipped it to a phantom DRIFT). Re-add a row when/if a successor is enabled. `ka-0000000874`. |

## Syncing

The enforcement mechanism (operator decision `ka-0000003376`, chosen 2026-08-24) is
**pointer stubs**, replacing the old manual `kd update -f prompt=…` copy-sync. There
are two states a live gardener prompt can be in, and the drift alarm accepts both:

1. **Pointer stub** (the target end-state — see below). The `.md` is the runtime
   source of truth; nothing to sync, nothing to drift.
2. **Inline copy** (legacy, for a gardener not yet flipped). Still needs the manual
   sync; the rules for doing that safely are in the collapsible note at the end of
   this section.

<a name="pointer-stubs-the-enforcement"></a>
### Pointer stubs (the enforcement)

A pointer stub is a short live `prompt` that tells the gardener to **read + follow its
own `gardenio/plans/gardener-prompts/<name>.md` from the freshly-cloned `main` working
tree** at cycle start, instead of carrying an inline copy of the runbook. Because every
`project=gardenio` gardener pod boots on fresh `main` (`initclone.go`: `git fetch --all`
+ `checkout main` + `pull --ff-only`), the `.md` resolves at runtime. Edit the `.md`,
merge — the next cycle reads it. The drift class cannot recur.

**Stub contract** (must satisfy `scripts/check-gardener-prompt-drift.py::_is_pointer_stub`,
or the drift alarm will red the gardener):

- the stub body **contains the literal string** `gardener-prompts/<name>.md` (its own
  runbook path), and
- the stub is **≤ 1500 bytes** (short — it delegates, it does not restate the runbook).

A recognized stub counts as **in-sync regardless of the `.md` body** — a stub cannot
diverge from the file it reads at runtime. Template (adapt the gardener name + one-line
role; keep it a pointer, never restate the runbook):

```text
You are gardenio **gardener-<name>** (<one-line role>).

Your full runbook for THIS cycle is the version-controlled file:

    gardenio/plans/gardener-prompts/<name>.md

Your pod boots on a fresh, up-to-date `main` working tree. At the START of every cycle:
1. Read that `.md` and follow it as your complete instructions.
2. That `.md` is the single runtime source of truth; this bead prompt is only a pointer
   (enforcement for bug ka-0000000874, decision ka-0000003376). Do NOT act from this stub alone.
3. If you CANNOT read that file, do NOT proceed silently — `gb squawk` the failure and `gb stop`.
```

**Flipping a gardener to a stub (prod-blast — do it one at a time):**

1. **Resolve the live bead BY ROLE, not by a pinned id.** Gardeners are periodically
   re-authored under new `vN` ids, so a pinned id in the table / manifest below goes
   stale and closed (as of 2026-08-25, 7 of the 9 originally-pinned beads were closed;
   the live successors run under new ids — e.g. `intake` → `ka-0000003841`,
   `release-owner` → `ka-0000003719`). Find the live one with
   `gb trigger list` (status=open, enabled=true) and match by role/title. Flipping a
   **closed** bead is a silent no-op.
2. **Confirm the `.md` is authoritative for THIS gardener first.**
   `diff <(kd show <bead> --json | jq -r .fields.prompt) gardenio/plans/gardener-prompts/<name>.md` —
   if the live prompt is **ahead** of the `.md` (live-only content), the `.md` is NOT
   authoritative: **port the live delta into the `.md` via MR and merge that BEFORE
   flipping**, or the flip destroys live tuning. (Measured 2026-08-25: `bugfix` live was
   +7.9 KB ahead of its `.md` — provenance-scoped budget, floor-at-1, gate-aware owner
   probe — none of it yet in repo. Do **not** flip bugfix until that is ported.)
3. **Save the pre-flip prompt** (`kd show <bead> --json | jq -r .fields.prompt > /tmp/<name>.preflip`)
   so the gardener is one `kd update -f prompt=@/tmp/<name>.preflip` from rollback.
4. `kd update <bead> -f prompt=@<the-stub-file>` (`@file` fails on a missing file; `"$(cat …)"` would expand to nothing).
5. **Verify one live cycle completes normally before flipping the next gardener.** Start
   low-traffic (`oss-sanitization`, weekly) and end with the hot dispatch/merge lanes
   (`bugfix`, `delivery`) — those flip **last** and only after other lanes have cycled
   cleanly on a stub, because they are the lanes that repair the fleet.

**Project-less fallback.** A gardener created **project-less** (no working tree) cannot
read a `.md` at runtime, so the pointer does not apply — inline its runbook into an
advice/bundle instead (advice `ka-0000006996`). The current fleet is all `project=gardenio`,
so every live gardener is pointer-eligible today.

<a name="runbook-delegating-gardeners"></a>
### Runbook-delegating gardeners (positive coverage, no inline `.md`)

Some gardeners do not follow the `gardener-prompts/<name>.md` pattern at all: their live
prompt delegates the **full charter to a versioned runbook** under
`gardenio/docs/runbooks/<x>.md`, read at cycle start (`git show origin/main:<runbook>`).
The intake **groomer** (`ka-0000003841` → [`docs/runbooks/intake-groomer.md`](../../docs/runbooks/intake-groomer.md))
is the canonical case. These gardeners have **no** `gardener-prompts/<name>.md` to diff, so
the pointer-stub rule (which keys off the `gardener-prompts/<name>.md` string and a 1500-byte
cap) does not apply — the real groomer prompt is ~1.8 KB and names a `docs/runbooks/` path.

To put such a gardener under **positive** drift coverage (monitored, not merely unmonitored),
give its `gardeners.manifest.json` row a **`runbook`** key naming the repo-relative runbook
path and **omit** the inline `.md`:

```json
{ "name": "intake-groomer", "bead": "ka-0000003841", "runbook": "gardenio/docs/runbooks/intake-groomer.md" }
```

`check-gardener-prompt-drift.py` then, for that row, asserts **(a)** the declared runbook
file exists in the repo and **(b)** the live prompt still references it (verdict `RUNBOOK`,
in-sync). If the delegation is silently broken — the prompt no longer names its runbook — the
alarm reds it as `DRIFT`; a missing runbook file is a loud `ERROR`. (`ka-0000002202`, design
`ka-0000003223` §3.)

<details><summary>Legacy inline-copy sync (only for a gardener not yet flipped)</summary>

Until a given gardener is flipped to a stub, its inline copy must still be synced by hand
when the `.md` changes. The bead↔file mapping is the machine-readable
[`gardeners.manifest.json`](gardeners.manifest.json). **The pinned bead ids drift — resolve
the live bead by role (step 1 above) before running `kd update`; a pinned id may be
closed.**

```bash
# Resolve <bead> by role first (gb trigger list). Then, for one gardener:
kd update <bead> -f prompt=@gardenio/plans/gardener-prompts/<name>.md
```

> ⚠ **Never blind-sync a gardener that carries live-only content** (a
> `diff <(kd show <bead> --json | jq -r .fields.prompt) <name>.md` with a
> `^[0-9]+d` pure-delete hunk). Port the live delta *into* the `.md` first, then
> sync — a blind overwrite wipes it (the trap that bit `ka-0000004099`). Verify a
> gardener is safe to sync with the drift script's diff, or run the alarm
> workflow, before overwriting. This is the same "confirm the `.md` is
> authoritative" check as step 2 of the flip.

</details>

## Design notes

See `docs/research/gardener-chain-review.md` (archived — excluded from this
export) for the rationale behind the mission-KPI structure and the hand-off
aversion rule. Cross-gardener hand-offs go through plain comments — label-based routing
(e.g. `needs-rescope`) was dropped per MR !1519 follow-up.

Summary of the pattern every gardener prompt follows:

1. **Mission KPI** (top of prompt) — a single-line metric that must move
   each cycle.
2. **Liveness-aware triage block** — shared verbatim across gardeners;
   prevents counting zombie agents as in-flight work.
3. **Per-cycle actions** — the lane-specific work loop.
4. **Hand-off policy** — prefer doing the small thing yourself; hand-off
   is a failure mode, not a clean hand-over.
5. **Wrap-up discipline** — every `gb stop` wrap-up reports the KPI delta
   for the cycle, not just "what I did."
6. **Out of scope** — the hard lines the gardener does not cross.

Concurrency: budgets are intentionally generous (≥5 crew per gardener).
Crew is cheap; serialized gardeners are the failure mode we are avoiding.
