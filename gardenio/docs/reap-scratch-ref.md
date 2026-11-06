# Reap-scratch-ref preservation (Tier 3 of `gb salvage`)

Bug: gd-PF42yiyH1d. Parent epic: gd-ILh55xdRYR (crew survivability).

`gb salvage` already had two preservation tiers before this work:

- **Tier 1** — bundle every dirty worktree (tracked + untracked) to S3, with a
  manifest, so `gb salvage restore` can round-trip it. Requires `COOP_S3_BUCKET`.
- **Tier 2** — an opt-in (`--push` / `GARDENIO_SALVAGE_PUSH`) force-push of a
  *tracked-only*, gitleaks-gated snapshot to `refs/heads/salvage/<agent-bead>/<target>`
  on the clone's origin.

Both tiers require configuration a crew pod may not have: no bucket, no
`--push`. In that default configuration a reap (OOM, spot reclaim, idle-out)
lost a dirty tree exactly as if `gb salvage` did not exist — this was the
gascity Stop-bug incident that opened gd-PF42yiyH1d.

## What Tier 3 does

`preserveReapScratch` (in `gardenio/controller/cmd/gb/salvage_reap.go`) runs
**unconditionally** on every `gb salvage` invocation that finds a dirty
target — independent of S3 bucket config, `--push`, and
`GARDENIO_SALVAGE_PUSH`. For each dirty target it tries, in order:

1. **Scratch-ref push.** Build a tracked-files-only snapshot commit (the same
   `snapshotTrackedOnly` helper Tier 2 uses — untracked files never leave the
   pod via this path), gate it through `gitleaks` against the durable remote
   anchor, and force-push it to:

   ```
   refs/gardenio/wip/<agent-bead-id>/<target-name>
   ```

   e.g. `refs/gardenio/wip/gd-agent1/work`.

2. **Diff-comment fallback.** If the push is impossible for any reason — no
   `origin` remote, an unborn `HEAD`, missing `gitleaks` on `PATH`, a leak
   found, or the push itself failing — attach a diff of the target (tracked
   **and** untracked files) as a comment on the **agent** bead, truncated to
   32KiB. A bead comment is Dolt-durable and readable by every holder of the
   shared beads token — a **wider** audience than the git ref it stands in
   for — so this fallback runs its **own** `gitleaks` gate over the diff
   before posting: a leak, or a missing `gitleaks` binary, withholds the diff
   entirely and instead posts a "WIP withheld" notice naming the dirty files
   (never their content). If no diff could be captured at all (e.g. the
   scratch-ref push failed and the fallback also found nothing to show), the
   comment says so explicitly ("UNRECOVERABLE") instead of staying silent.

### Where Tier 3 runs in the sequence (gd-nKOOfEvKLt)

`runSalvage` orders the tiers by fidelity, not by number:

- **Bucket configured:** Tier 1 (S3 bundle) runs first, then Tier 3, then
  Tier 2 if `--push` is on. The bundle carries tracked *and* untracked files
  and round-trips through `gb salvage restore`, so it gets first call on the
  `--timeout=15s` preStop grace budget; Tier 3 runs on whatever is left. Tier 3
  used to run first and could spend the whole budget on a slow push or a
  full-history `gitleaks` scan, so the bundle hit the deadline before its first
  PUT (PR #10 review item 3).
- **No bucket:** Tier 3 runs first, with the whole budget — it is the primary
  sink — then Tier 2 if `--push` is on.

Tier 3 is unconditional in both shapes: it runs when every bundle uploaded,
and when the S3 client could not even be built (credentials unresolvable, S3
down). Tier 2 likewise no longer stops when the S3 client fails — the remote
is then the only durable sink.

Either outcome — a pushed ref, a posted diff, a withheld notice, or an
UNRECOVERABLE notice — is recorded on the agent bead as `wip_reap_ref` and/or
`wip_reap_comment`, which `gb session death-report --bead <agent-bead>`
surfaces under "Uncommitted work preserved on reap". A clean tree (no dirty
target discovered) sets neither field and creates neither a ref nor a comment.

### No `mr_url` skip, in either push tier

Neither the Tier 2 push nor Tier 3 skips a target because the worked task
already has an `mr_url`. An MR branch holds only *committed, pushed* work;
uncommitted edits and commits made since the last push are on no remote, and
those are exactly what salvage exists to save. (Tier 2 used to skip its whole
push when an `mr_url` was present, which left a dirty tree on an MR task
unpreserved whenever Tier 3 was not also reachable — gd-Jpw0LU1Rgu.) Both tiers
run the same `pushTrackedSnapshot` helper; only the destination ref and the
"nothing to carry" pre-check differ.

## The `refs/gardenio/wip/` namespace

`refs/gardenio/wip/` is deliberately **not** `refs/heads/` or `refs/tags/` —
those are the only two namespaces GitHub/GitLab's default branch/tag UI,
`git branch -r`, and `git ls-remote --heads` / `--tags` walk. A ref under
`refs/gardenio/wip/` is therefore excluded from the repo's normal branch
listing **by construction**, not by server-side filtering — no branch
protection rule, CI trigger, or code owner review ever sees it, and it can
never collide with a real branch name.

It is still a fully addressable ref on the origin remote:

```sh
# list every scratch ref
git ls-remote origin 'refs/gardenio/wip/*'

# fetch one and inspect it
git fetch origin refs/gardenio/wip/gd-agent1/work
git log -p FETCH_HEAD

# recover it as a local branch
git fetch origin refs/gardenio/wip/gd-agent1/work:recovered-wip
git checkout recovered-wip
```

Each push is a **force** push (`+<sha>:refs/gardenio/wip/...`) scoped to one
`<agent-bead-id>/<target-name>` pair, so a pod that reaps more than once
(e.g. repeated OOM restarts of the *same* agent bead) simply overwrites its own
prior scratch ref with the latest snapshot. That only bounds sprawl **within**
one agent bead, though: the agent bead id is minted fresh per spawn, so every
crew that ever reaps with a dirty tree leaves its own permanent
`refs/gardenio/wip/<bead>/<target>` ref behind — there is currently **no
TTL or prune path** for refs whose agent bead has long since gone away. Treat
the namespace as unbounded until a pruning job exists; `git ls-remote origin
'refs/gardenio/wip/*'` is how to audit its current size.

**Isolation is convention, not enforcement.** The `<agent-bead-id>` path
segment keeps one crew from routinely colliding with another's ref, but it is
not an access boundary: any credential with push access to `origin` (which
every crew pod's PAT has — `contents: write` covers arbitrary ref paths, and
branch-protection rulesets cannot target a non-`refs/heads/` namespace) can
force-push over *any* other agent's scratch ref. Do not rely on this namespace
to keep one agent's WIP private from another.

**Never** treat this namespace as a source of truth for landed work: it is a
best-effort forensic trail for a pod that died with uncommitted changes nobody
got to push properly. Real work still belongs on a real branch behind an MR.

## Recovering from the diff-comment fallback

When the scratch-ref push could not happen and a diff was safe to post, the
agent bead carries a comment from `gardenio-salvage` with the diff, capped at
32KiB. The comment states whether it was truncated to fit that cap — a
truncated diff is not guaranteed to `git apply` cleanly (a cut mid-hunk), so
treat it as a forensic reference to recover the change by hand rather than a
patch to apply blind:

```sh
kd show <agent-bead-id>   # find the comment, read the diff block
git apply <<'EOF'         # try it — works when the diff was not truncated
<paste the diff here>
EOF
```

A comment that says "WIP withheld" or "UNRECOVERABLE" instead carries no diff
content by design — the gitleaks gate could not clear it, or nothing could be
captured. It names the affected files so the operator at least knows what was
lost; the content itself did not survive the pod.

## Related

- `gardenio/controller/cmd/gb/salvage.go` — implementation (see the file
  header comment for the full Tier 1/2/3 design).
- `gb session death-report` — surfaces `wip_reap_ref` / `wip_reap_comment`
  (`gardenio/controller/internal/deathreport/report.go`).
