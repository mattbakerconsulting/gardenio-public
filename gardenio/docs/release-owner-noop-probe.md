# release-owner no-op probe — the Step 1.5 predicate as a versioned script

> **Retired (2026-10-31).** The `gardener-release-owner` lane is retired, and the script and its test were deleted with `gb release status` (plan gd-UZKZ9H25UD step 3, gd-V9ukTsRAAJ); read releases with `kd release list --project <p>` ([`releases-design.md`](releases-design.md)). Kept for history; the links below point at files that no longer exist.

**Script:** `scripts/release-owner-noop-probe.sh` (deleted)
· **Test:** `scripts/release-owner-noop-probe_test.sh` (deleted)
· **Task:** ka-0000006139 (increment 2 of plan ka-0000000544, "Probe pods")
· **Ported from:** Step 1.5 of doc ka-0000000327 (gardener-release-owner cycle procedure)
· **Script bead:** ka-0000006139's child (see [Reuse](#reuse-by-other-gardeners-and-the-probe-target))
· Operator: slack:U0OPERATOR1

## Why this exists

~85% of `gardener-release-owner` cycles cannot cut a release, because `main`
has not moved since the latest tag. Today that conclusion costs an Opus gardener
pod (2 CPU / 8Gi, ~9 turns, ~800k cache-read tokens) — spun up only to run a
predicate that is **already written as shell inside the gardener prompt** and
then decide "nothing to do".

This script *is* that predicate, extracted into a standalone, versioned,
reviewable, reusable unit. Run it in a cheap 50m-CPU probe pod (or locally) and
only wake the gardener when it exits non-zero.

The trigger engine's condition DSL cannot express this: every condition leaf is
bead-store-scoped (no `os/exec`, no `net/http`), so it can ask "how many open
beads match X?" but not "has `main` moved?". That gap is the entire reason the
"Probe pods" plan (ka-0000000544, narrative ka-0000002893) exists. This script
is the release-owner-specific predicate that the increment-1 probe
`execution_target` (ka-0000002769) will execute.

## The predicate

Suppress the gardener (**no-op, exit 0**) if and only if **all four** hold:

| # | Condition | Meaning |
|---|---|---|
| 1 | `TAG_SHA == MAIN_SHA` | `main` has **not** moved since the latest tag |
| 2 | `$LATEST` does not end in `-rc` | no red release-candidate to drive |
| 3 | `OPEN_RELEASE_BLOCKER == ""` | no open release-blocker escalation |
| 4 | `BLOCKING_DECISIONS_COUNT == 0` | no open release decision |

If **any** condition is false there is real work → **wake the gardener**
(exit 1). Condition 1 is checked first and short-circuits before the two bead
reads, so the common cut case (`main` moved) is decided from three
`git ls-remote` values alone.

## Inputs

All inputs are environment variables. Each is **auto-derived** from the live
repo/daemon when unset, and **injectable** for reuse by another gardener or for
hermetic testing. The three SHA/tag reads are cheap `git ls-remote` calls (no
fetch, no local objects — works inside the narrow main-only agent clone).

| Env var | Default (when unset) | Notes |
|---|---|---|
| `REMOTE` | `origin` | git remote to probe |
| `LATEST` | newest calver tag via `git ls-remote --tags` | e.g. `2026.215.3` or `…-rc` |
| `TAG_SHA` | commit sha of `$LATEST` via `git ls-remote` | annotated tags are peeled (`^{}`) |
| `MAIN_SHA` | sha of `refs/heads/main` via `git ls-remote` | |
| `OPEN_RELEASE_BLOCKER` | id of an open release-blocker escalation, else `""`, via `kd list --type=escalation` | set to `""` to assert "none" without the read |
| `BLOCKING_DECISIONS_COUNT` | `.blocking_decisions \| length` from `gb release status --json` | **not** a `kd list --type=decision` filter — pre-ka-0000003942 decisions carry no project label and read as a false all-clear |

## Outputs

**stdout** — exactly one machine-readable verdict line:

| stdout | Meaning |
|---|---|
| `noop` | all four conditions hold — suppress |
| `wake:main-moved` | condition 1 false |
| `wake:rc-tip` | condition 2 false |
| `wake:release-blocker` | condition 3 false |
| `wake:blocking-decision` | condition 4 false |
| `error:<reason>` | an input could not be derived |

**stderr** — a human-readable per-leaf breakdown (the derived values and which
condition tripped).

**exit code** — the contract the probe `execution_target` keys on
(`PhaseToAgentState`: PodSucceeded→done, PodFailed→failed):

| exit | Meaning | Action |
|---|---|---|
| `0` | no-op confirmed | **suppress** the gardener spawn |
| `1` | real work available | **wake** the gardener |
| `2` | indeterminate / input error | **fail-open**: wake the gardener anyway |

Both `1` and `2` mean "spawn the gardener", so a **broken probe fails open** —
the plan's explicit safety property: a gated trigger that silently never fires
is indistinguishable from a dead one. The distinct codes and verdict strings
exist for humans and observability (increment ka-0000004863).

## Usage

```sh
# Live, against the real repo + daemon (auto-derives everything):
scripts/release-owner-noop-probe.sh; echo "exit=$?"

# Reuse for another remote / injected state (e.g. from a probe pod):
REMOTE=origin LATEST=2026.215.3 \
  TAG_SHA=abc… MAIN_SHA=abc… \
  OPEN_RELEASE_BLOCKER="" BLOCKING_DECISIONS_COUNT=0 \
  scripts/release-owner-noop-probe.sh
```

## Verification

`make lint` runs the hermetic test (`…_test.sh`, 10 cases, all inputs injected —
no network) plus a live no-fixture invocation. Beyond that, the predicate was
exercised end-to-end (task ka-0000006139 verify):

- **Live `main` (moved):** against the real repo at a moment when
  `main_commits_ahead=2` → `wake:main-moved`, exit 1. ✓
- **Live no-op:** `MAIN_SHA` pinned to the latest tag with **live** `kd`/`gb`
  bead reads for conditions 3+4 → `noop`, exit 0. ✓
- **Stale-branch git fixture:** a throwaway bare remote whose `refs/heads/main`
  sits exactly on tag `2026.100.1` (real `git ls-remote` derivation, no SHA
  injection) → `noop`, exit 0; then a commit pushed past the tag → moved →
  `wake:main-moved`, exit 1; then an `-rc` tip → `wake:rc-tip`, exit 1. ✓

## Reuse by other gardeners and the probe target

The script is deliberately backend-agnostic: it reads only env vars and the two
CLIs (`git`, `kd`, `gb`) that exist in the slim probe image. Other gardeners that
share the "has `main` moved since the last tag?" gate can reuse it directly by
overriding `REMOTE`/`LATEST` (the bead-read leaves are release-specific and can
be forced clean via the two env sentinels). The increment-1 probe
`execution_target` (ka-0000002769) runs it as `/bin/sh -lc "$SCRIPT"` and maps
its exit code to done/failed with no OAuth token and no model on the pod.

The script content is also mirrored into a data-plane bead (child of
ka-0000006139) so it is discoverable and re-executable from beads. **Note:**
`type=script` is not yet a builtin bead type — it is introduced by increment 1
(ka-0000002769). Until it lands, the bead is filed as `type=chore` and flagged
for reclassification to `type=script`; the canonical, diffable source of truth
is this file in-repo.
