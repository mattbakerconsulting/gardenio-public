# gVisor rc=128 — zero-canary on-node discriminating check (LIVE half)

**Bug:** [ka-0000000357](https://gardenio.e2e.example.com/beads/ka-0000000357) ·
**Steer:** [ka-0000008062](https://gardenio.e2e.example.com/beads/ka-0000008062) (gardener req-1) ·
**Script:** [`discriminating-restore-check.sh`](./discriminating-restore-check.sh)

## What this settles

The dir-medium thaw `rc=128` regression fix is **merged** (MRs !4910 + !4914): the
pre-restore lower-only CWD-dentry assertion is now gated on the overlay medium
(dir-medium skips it; self-medium keeps it verbatim), plus a post-restore
workspace-liveness guard keeps dir-medium fail-closed against real loss.

Read-only on-node evidence already supports Fix B (the dir filestore
`/var/lib/gardenio/overlay` is empty on disk; the workspace content lives in
`checkpoint.img`/`pages.img`). This script is the **LIVE half** the gardener steer
requires *before* spending the project's ONE canary on a gated release: actually
replay `runsc --overlay2=root:dir restore` of the already-persisted **failing**
freeze bundle and prove the workspace round-trips.

| Outcome | Meaning | Next step |
|---|---|---|
| **PASS** (`restore_rc=0` + populated CWD) | runsc rebuilds `/home/agent/workspace` from `checkpoint.img`; the pre-restore assertion was a false-negative. Fix B is **safe**. | Cut the gated release (operator step 2), then re-drive ONE canary (ka-0000001355). |
| **FAIL** (`restore_rc!=0` or CWD absent) | The loss is **real**; the medium-gate would mask corruption. | **HALT.** Do not release. Reopen ka-0000000357 toward freeze-side quiesce/sync. |

## What CI covers vs. what still needs the on-node operator run

This check has **two halves**, and being honest about the split is the whole point of
task [ka-0000002792](https://gardenio.e2e.example.com/beads/ka-0000002792) — there is
**no silent cap**:

| Half | What it is | Where it runs | Gated? |
|---|---|---|---|
| **Verdict ladder + parser** (pure) | `drc_verdict` (the PRE-FIX=FAIL / skeleton-only=HOLLOW / skeleton+stacking=PASS classification + exit code) and `build_lowers` (the DEFECT-A `.overlay-opts` parser) | **CI**, every MR/main push — checks.yml `script-selftests`, driven by [`discriminating-restore-check_test.sh`](./discriminating-restore-check_test.sh) with synthetic measurements (no runsc/mount/kubectl/node/network) | ✅ **real gate** — a non-zero exit fails the `CI / Test` status check; no `allow_failure`, no skip |
| **Live count-gated barrier restore** | the actual `runsc --overlay2=root:dir restore` of a persisted dir-medium freeze bundle + the in-sentry CWD/HOLLOW asserts that PRODUCE the measurements | **operator on-node only** — needs a privileged prod gVisor node (`america-e2e-eks`); the sandboxed agent SA cannot create the probe pod or reach a node | ❌ not CI-able — see "How to run" below |

**Why the split is real and not a cop-out.** The ladder logic is what silently
regressed before: a wrong `.overlay-opts` parse (DEFECT-A) once left the rootfs
unmounted and made the restore `rc=128` look like data loss; a CWD-only liveness check
(DEFECT-C) once green-lit a HOLLOW `/usr`. Those are **pure classification bugs** — they
do not need a live node to catch, and now they can't ship: the hermetic test pins all
five rungs, the conjunctive PASS gate, the HOLLOW-before-FAIL precedence, and the
parser's three `.overlay-opts` shapes. What CI **cannot** synthesize is whether `runsc`
*actually* rebuilds the upper from `checkpoint.img` under `--overlay2=root:dir` — that is
the load-bearing empirical question the live half answers, and it stays an operator run.

So a green CI gate means **"the verdict machinery is sound"**, *not* **"the canary will
pass"** — the operator must still drive the on-node check (below) and read its
`RESULT=` line before cutting the gated release.

The CI gate also drift-checks the embedded one-apply Job manifest
([`discriminating-check-job.yaml`](discriminating-check-job.yaml))
against its source-of-truth script, so the two can never silently diverge.

## Why a raw `runsc restore` *is* the "assertion bypassed" check

The assertion that fail-closed prod is **controller-side bash** (`remount_rootfs`
POST-MOUNT CWD-dentry test, `freezethaw_node.go` ~L1090-1140). A standalone
`runsc restore` invoked directly never runs that bash — so it inherently bypasses
the gate; there is nothing to patch out. The only question left is the load-bearing
one: **does runsc itself rebuild the rootfs-overlay upper (incl. the workspace) from
`checkpoint.img` under `--overlay2=root:dir`?** This script answers exactly that, then
asserts liveness the same way the merged post-restore guard does
(`FT_DIRMEDIUM_CWD_LIVE`).

## The count-gated N-container barrier (topology fix, ka-0000007097)

A persisted pod checkpoint is **one image** with `container_count=N` baked into
`checkpoint.img`. It is NOT restorable by replaying the **agent sub-container alone**:
the bundle carries both a `sandbox/` and an `agent/` subdir, and `runsc restore` of the
agent sub reads the agent spec's `io.kubernetes.cri.sandbox-id` annotation and tries to
**join that sandbox**, whose `<cid>_sandbox:<cid>.state` file only exists once the
sandbox root has itself been restored under the same `--root`. Restoring agent-only
therefore fails-closed with:

```
cannot load sandbox: open <cid>_sandbox:<cid>.state: no such file or directory  (rc=128)
```

That `rc=128` is a **script-topology artifact, not a Fix-B FAIL** — it tells you nothing
about whether the workspace round-trips. To produce a valid PASS/FAIL the script mirrors
the proven two-phase barrier in production thaw (`thawTmpl` `[5]`,
`freezethaw_node.go`) and the spike PoC
(`multi-container-resume-node.sh`, in the archive tag `archive/clutter-pre-delete-2026-10-28`; write-up in
[`multi-container-resume.md`](../../gardenio/docs/gvisor-runsc-cr/multi-container-resume.md)):

1. **Allocate two scratch cids** — `SBCID` (sandbox) + `RCID` (agent) — and copy **both**
   `sandbox/` and `agent/` subdirs into the scratch bundle; mount fresh scratch overlays
   for each rootfs (the existing `remount()` already iterates `sandbox agent`).
2. **Repoint the `sandbox-id` annotation** in the scratch `agent/config.json` (and
   `sandbox/config.json`) from the original live sandbox cid to `SBCID`, so the scratch
   agent links to the scratch sandbox rather than the long-gone live one.
3. **Phase 1 — restore the sandbox FIRST, backgrounded.** With `--detach` the call does
   not return; the sandbox parks at `restoringUnstarted`, reading the baked count.
4. **Phase 2 — restore the agent sub in the foreground.** Joining it satisfies the count
   and unblocks both; `wait` collects the sandbox rc. A `FAIL` now requires **either**
   `restore_rc!=0` **or** `sandbox_rc!=0`.

> **No `--container-count` flag.** The barrier target is read by `runsc` from
> `checkpoint.img`; it is **not** a CLI flag in this gVisor build. Neither `thawTmpl` nor
> the PoC passes one, and this script does not either — passing an unknown flag would
> itself rc!=0 and re-introduce a spurious FAIL.

The post-barrier liveness assertion is unchanged: `runsc exec $RCID test -e $CWD` + an
entry count inside the restored sentry, exactly as `thawTmpl` `[6b]`
(`FT_DIRMEDIUM_CWD_LIVE`) does on the agent cid.

## Safety / blast radius

- **Read-only w.r.t. prod.** Restores into **scratch** cids (sandbox + agent) against
  **copies** of the bundle/overlay under `/var/tmp`. Never mutates the live checkpoint
  image, never deletes a pod, never reads or flips `gvisor.freezeThaw`.
- Cleanup (on EXIT) deletes only its own scratch cids + `/var/tmp` working dir.
  Respects the HARD HOLD: ZERO canaries, ZERO orphans.

## How to run

Requires privileged node access — the agent SA (`gardenio-gvisor:gardenio-agent`)
cannot create pods or reach nodes, so this runs by an operator with a prod kubectl
context (`america-e2e-eks`, acct `234567890123`).

### Primary path — ONE apply (self-contained Job)

The script is embedded in a self-contained ConfigMap+Job manifest
(`scripts/gvisor/discriminating-check-job.yaml`, **generated** from
this script by `scripts/gvisor/gen-drc-job.sh` so it can never drift). No
`kubectl cp`, no exec dance — apply it and read the RESULT line from the logs:

```sh
# pk8 canary ka-0000005316 froze agent "gardenio-uuae" on node ip-10-30-136-242.
# Override the bundle if reaped:  kubectl -n gvisor-poc set env \
#   job/gvisor-discriminating-check AGENT=<name>   (or edit the env: in the yaml)
kubectl apply -f scripts/gvisor/discriminating-check-job.yaml
kubectl -n gvisor-poc logs -f job/gvisor-discriminating-check   # -> RESULT=PASS|FAIL|INCONCLUSIVE
```

The Job sets `backoffLimit: 0` (a privileged restore op is never retried) and
`ttlSecondsAfterFinished: 1800` (self-cleans 30m after completion). Force-clean
early if you like: `kubectl -n gvisor-poc delete job gvisor-discriminating-check`.

### Fallback — manual privileged probe pod

If you'd rather drive it interactively (e.g. with `--keep` to inspect the scratch
sandbox), use the long-lived probe pod:

```sh
kubectl -n gvisor-poc apply -f scripts/gvisor/probe-pod.yaml
kubectl -n gvisor-poc wait --for=condition=Ready pod/gvisor-probe --timeout=180s
kubectl -n gvisor-poc cp scripts/gvisor/discriminating-restore-check.sh \
    gvisor-probe:/host/var/tmp/discriminating-restore-check.sh
kubectl -n gvisor-poc exec gvisor-probe -- \
    chroot /host /bin/bash /var/tmp/discriminating-restore-check.sh --agent gardenio-uuae
kubectl -n gvisor-poc delete pod gvisor-probe --grace-period=0 --force
```

If the ~5-day-old bundle/image was already reaped (`SETUP-ERROR: bundle dir absent`),
re-drive **ONE freeze** on a fresh `gvisor-poc` agent to regenerate a persisted
bundle, then run this against it — still **zero thaw, zero canary**.

## Output contract

- `RESULT=PASS  restore_rc=0  cwd='…'  entries=N` → exit 0
- `RESULT=FAIL  …` → exit 1 (HALT; do not release)
- `RESULT=INCONCLUSIVE  …` → exit 1 (flaky exec / empty CWD — re-run, do not treat as PASS)
- `SETUP-ERROR: …` → exit 2 (bundle/image/runsc missing — fix setup; **not** a B-verdict)
