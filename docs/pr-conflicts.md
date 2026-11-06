# PR conflict fields

A pull request's merge conflict with `main` is a fact on its bead, not only a
comment on the GitHub PR. Triggers fire on bead fields, so a conflict written
to the bead can wake the agent that owns the PR, start the mechanical resolver,
and tell the merge desk and gardeners before they pick the PR up (plan
gd-nTCENSwMOH, "Conflict-aware merges").

The fields are declared in `mrFields` (`kbeads/internal/server/config.go`), so
every issue-kind type that carries `mr_url` accepts them: the MR bead and the
task bead linked to it. Like the other `mr_*` fields they are strings, because
the bridges write a uniform string map. A writer sets a field only when its
value changes, never on every scan (bead history bloat, gd-R4Rs0dJBC5).

| Field | Writer | Values | Cleared |
|---|---|---|---|
| `mr_merge_state` | github-bridge conflict scanner (Task 2.2, gd-RjkmIPXOgN) | `clean`, `conflicting` or `unknown`. `unknown`: the scan failed or the head could not be fetched. Empty: never scanned. | Never cleared; each scan that changes it overwrites it. |
| `mr_conflict_files` | conflict scanner (2.2) | JSON array of repo-relative paths, sorted, at most 50 entries. When capped, the last entry is `"…+N more"`. | Set to empty when the PR goes `clean`. |
| `mr_conflict_kind` | conflict scanner (2.2) | `mechanical` when every conflicting file is on the resolver's mechanical list (Task 3.2, gd-mBJXcHcBxb), otherwise `hand`. | Set to empty when the PR goes `clean`. |
| `mr_conflict_since` | conflict scanner (2.2) | RFC3339 time the PR first became `conflicting` in this episode. Kept while it stays conflicting. | Set to empty when the PR goes `clean`. |
| `mr_conflict_base` | conflict scanner (2.2) | The `main` SHA the scan merged the PR head against. | Set to empty when the PR goes `clean`. |
| `mr_conflict_cause` | conflict scanner (2.2) | `<sha>[ #<pr>]`: the first `main` commit since the PR's last clean scan that touched a conflicting file, with its PR number when it has one. Empty when unknown. | Set to empty when the PR goes `clean`. |
| `mr_overlaps` | open-PR overlap scan (Task 4.1, gd-Lnfac5eRIH) | JSON array of `{"pr":N,"files":[...]}`, one entry per other open PR whose head conflicts with this one. | Set to empty when no other open PR conflicts with it, or when the PR closes or merges. |

## The scanner

github-bridge's conflict scanner (`gardenio/controller/cmd/github-bridge/conflicts.go`)
writes the first six fields. It keeps a bare, blobless mirror of `main` and every
`refs/pull/*/head`, and for each open PR based on `main` runs
`git merge-tree --write-tree --name-only main <head>`: exit 0 is `clean`, exit 1
is `conflicting` with the listed paths, and anything else (or a head that will
not resolve) is `unknown`. merge-tree fetches only the blobs of paths both sides
changed. The mechanical list is `MechanicalConflictPaths` in the same file.

When it scans: a full scan on start, every `CONFLICT_SCAN_INTERVAL` (5m), and
60s after the last push to `main`; one PR on a `pull_request` opened, reopened
or synchronize. `mr_conflict_cause` is computed only when a PR goes from clean
to conflicting while the bridge is up, because the last clean base is kept in
memory.

| Setting | Default | Meaning |
|---|---|---|
| `CONFLICT_SCAN_REPO` | unset (scanner off) | `owner/name` of the repo to scan. |
| `CONFLICT_SCAN_DIR` | `/var/lib/github-bridge/mirror` | Mirror directory; an emptyDir. |
| `CONFLICT_SCAN_INTERVAL` | `5m` | Full-scan period. |
| `CONFLICT_SCANNER` | on | `off` disables it. |

It also needs the GitHub App (the token reads `contents`) and a `git` binary on
`PATH`; without either it logs why and stays off. The token reaches git through
`GIT_ASKPASS`, which is the bridge binary itself, so it never appears in argv or
a URL.

Metrics: `github_bridge_prs_conflicting{kind}`, `github_bridge_conflict_scan_seconds`,
`github_bridge_conflict_scan_errors_total`, `github_bridge_conflict_prs_without_bead`,
`github_bridge_pr_overlap_pairs`.

## The mechanical resolver

When a scan finds a PR `conflicting` with `mr_conflict_kind=mechanical`, the
scanner hands it to the resolver (`resolve.go`, same directory; Task 3.2,
gd-mBJXcHcBxb), which pushes one merge-of-main commit to the PR branch. It is
deterministic (no LLM). Each mechanical pattern has a rule in `resolveRules`:

| Paths | Rule |
|---|---|
| `gardenhub/testdata/jsbudget/*.txt`, `scripts/ratchets/line-budget.d/*.tsv` | Union of lines: main's lines, then the PR's lines main lacks, in order. Both sides must have the file. |
| `gardenhub/testdata/golden/**` | Main's version wins. Goldens never enter a feature PR (gd-oxDhsqdFVP). |

Any other conflicted file aborts. The PR is `hand` and Task 3.1 owns it.

The merge is built without a worktree. `git merge-tree --write-tree` gives the
merged tree and each conflicted path's stages. A temporary index rewrites those
paths by their rule, so only their blobs are fetched, and `commit-tree` makes
the merge with the PR head as first parent and main as second. The commit is
authored as `CONFLICT_RESOLVER_AUTHOR` (default the `gardenio-io[bot]` user).
Before the push the resolver checks three things: `git diff --check` on the
resolved files is clean, `git merge-tree` against main returns 0, and the
merge adds no path to the PR's diff against main that the PR did not already
change. If any check fails, the result is `verify_failed`. The push is
`--force-with-lease=refs/heads/<branch>:<scanned head>`. If a crew push races
it, the lease fails and nothing is overwritten (`lease_lost`). The resolver
then comments on the PR, naming each file and its rule.

It skips a fork PR, a PR labelled `no-bot-merge`, and a PR whose head moved
since the scan. It also skips any (main, head) pair it has already tried. It
makes at most one bot merge per PR per main SHA, and never an empty commit.
The push runs the PR light tier only. The push token is minted apart from the
bridge's read token, with `ConflictResolverTokenPermissions`: `contents: write`
for the push and `workflows: write`, because GitHub refuses a push that carries
main's workflow edits without it. Comments and PR reads stay on the bridge
token. `CONFLICT_RESOLVER=off` disables the resolver.

Metric: `github_bridge_mechanical_resolves_total{result,reason}`, where result
is `ok`, `aborted`, `lease_lost` or `verify_failed`. An `aborted` result carries
its `reason`: `merge_failed` (merge-tree errored), `already_clean`,
`hand_conflict` (a conflicted file has no rule), `build_error` (writing or
committing the resolved tree failed) or `push_refused` (refused with the branch
still at the scanned head). Other results have an empty reason.

The resolver writes its temporary index under the mirror directory, the pod's
one writable volume (the root filesystem is read-only), and writes the tree with
`--missing-ok`, so the blobless mirror fetches only the conflicted blobs
(gd-qDrQYJB726).

## Overlaps between open PRs

After each full scan, the overlap pass (`overlaps.go`, same directory) writes
`mr_overlaps`. It takes the PRs that are `clean` against `main`, reads each one's
changed files (`git diff --no-renames --name-only main...<head>`), and runs
`git merge-tree` on each pair whose file sets intersect. Pairs with no file in
common cannot conflict, so they are not merged; the pass logs how many merges
it ran. Exit 1 is an overlap, recorded on both PRs' beads. Mechanical paths are
left out of both the intersection and the result, so two PRs that conflict only
on a golden or a jsbudget file do not overlap.

Each entry's `files` is sorted and holds at most 10 paths; when there are more,
the last entry says how many were left out. A `conflicting` PR gets an empty
`mr_overlaps`, because it needs a rebase before its overlaps mean anything. An
`unknown` PR, or one whose diff or merge failed, keeps its current value. A
single-PR scan leaves `mr_overlaps` as it is. When a PR closes, the next full
scan clears it on that PR's beads. This works only for PRs that already had
overlaps while the bridge was up, because that list is kept in memory. As with
the other fields, a bead is written only when its value changes.


## Waking the PR's owner

When a PR goes conflicting on files that need a hand merge, the agent that
owns it is told within one trigger-engine cycle of the scanner's write, rather
than at the next pr-sweeper pass or desk send-back (Task 3.1, gd-xTJWewKG71).
Mechanical-only conflicts are left to the resolver (Task 3.2; ruling on
gd-79RbYR7jKS).

It is one bead trigger with the `wake_pr_owner` action
(`gardenio/controller/internal/bridge/trigger_pr_conflict_wake.go`; reference
in [`gardenio/docs/triggers.md`](../gardenio/docs/triggers.md#wake_pr_owner)):

```bash
gb trigger create bead \
  --title 'PR conflict: wake the owner (hand conflicts)' \
  --event-types updated \
  --condition 'mr_merge_state:changed_to:conflicting' \
  --condition 'mr_conflict_kind:==:hand' \
  --condition 'type:in:task,bug,feature,chore' \
  --action wake_pr_owner --agent-project gardenio --max-concurrent 20
```

The type condition keeps the MR bead, which carries the same fields, from
firing a second wake for the same PR. `--max-concurrent` bounds fires per
engine cycle, so it is set above the number of PRs one push to `main` can
break at once.

Who is told, the first that applies: the task's live crew agent (a mail
nudge); else the gardener of the task's plan (nudged when live, woken in place
when parked); else nobody. Every fire also comments the message on the task,
naming who was told. The message:

> PR #N now conflicts with main (since `<mr_conflict_since>`) on: `<files>`.
> First conflicting main commit: `<mr_conflict_cause>`. Merge main into your
> branch, resolve, push; do not wait for review. (`<task id>`, `<mr_url>`)

At most 10 files are named, then "and N more" (counting the scanner's own
`…+N more` entry).

One wake per conflict episode: `changed_to` matches only the write that moves
`mr_merge_state` to `conflicting`. Later writes in the same episode (new files,
a new base) leave it unchanged and do not match. A PR that goes `clean` and
then conflicts again is a new episode and wakes again. So does one whose scan
went `unknown` in between, since `unknown` also changes `mr_merge_state`.
