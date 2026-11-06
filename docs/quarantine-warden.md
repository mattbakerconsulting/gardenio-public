# Quarantine warden: the fix and dequarantine lane

The owner, 2026-11-05: "Flaky tests should be quarantined, we should add a rule. Also we
need a lane for managing dequarantine and fixes."

The rule is in `.github/known-failures.md` ("Quarantine") and the root `CLAUDE.md` ("Flaky
tests"): a test that goes red on a change that could not have caused it is quarantined the
first time anyone sees it, on a PR, a release tag or the nightly, and nobody re-runs it to see
if it goes green. This file is the lane that keeps that list from becoming a graveyard. It has
two parts:

| part | what | bead |
|---|---|---|
| the warden | a schedule trigger: quarantines what the readers missed, takes rows off once their fix is proven, renews or nags expired rows, and keeps every row's bug in the fix plan | trigger `gd-5s3DSnR9iS` |
| the fix plan | a standing plan whose steps are the quarantined tests' bug beads; its plan gardener crews them like any other plan | plan `gd-irrbwnRcsR`, gardener `gd-2d3xpzBdhS` |

The warden never fixes a test and never merges anything but its own list-only PR. The fix
plan's crews never touch a row they are not fixing. A fix PR deletes its own rows, as the
"Leaving" rule says, and the warden removes the rest once the evidence is in.

## Evidence: `scripts/quarantine.sh verdicts`

Every quarantined test still runs. The `go` rows run in each `build + test` job's
`quarantined tests (advisory)` step, which runs with `-v`, so each test prints a `--- PASS`
or `--- FAIL` line. The `golden` rows render and diff in `make test-garden-goldens`, where
golden.Check logs `quarantined <shot> passed` or `QUARANTINED <shot>: ...`.

`scripts/quarantine.sh verdicts LIST LOG...` reads those lines, whether from raw job logs or
from `gh run view --log` output, and prints `kind name bead pass|fail` for each row of LIST
that the logs report on. One fail anywhere in the logs makes the verdict `fail`.

One gap is known: a quarantined `*UnderChromium` row is skipped in the gardenhub browser job
and has no advisory run, so it never gets a verdict. Its bead's fix PR must prove it
(fix-plan step, below).

## What one run does

| step | what |
|---|---|
| 0 collect | main's list; the last 5 completed `ci.yml` runs on calver tags plus the newest nightly (`ci.yml`'s own 10:43Z `schedule` run; a tag skips the browser suite when no UI path changed, so the nightly is often the only golden and Chromium evidence); each run's `build + test (*)` and `gardenhub browser (chromium)` job logs; the fix plan's steps |
| 1 intake | a red test in those runs that has no row gets one (with a bug bead) in this run's PR |
| 2 dequarantine | a row whose bead is closed: removed when its test passed in every run since the bead closed (at least one), else its bead is reopened with the failing run |
| 3 expiry | a row past its expiry that failed in the window: renewed (+14 days, the fresh run as evidence); one that did not fail: left as it is, with one comment on its bead |
| 4 lane | every open bead named by a row is a step of the fix plan, or a step of another live plan, with the "Quarantine lane" note in its description; a bead orphan-parked `blocked` (its desk-run parent closed) is unparked |
| 5 PR | all of 1-3 in one list-only PR, opened ready with auto-merge |
| 6 record | `quarantine_health` on its own agent bead |

An open bead's row is never removed by the warden, however often it passes: a flake that fails
one run in twenty passes five in a row more often than not. Its fix PR removes it, or a crew
that proves the cause is gone closes the bead, and step 2 removes it on the next run.

## Fix plan

A standing plan, `plan:quarantine`. It is built like the general bugfix plan: no phases,
each step is a bug bead whose `parent-child` edge points at the plan epic, and an
event-driven plan gardener wakes on any change in the subtree. A step's crew:

- fixes the cause, never the symptom: no longer sleeps, no wider tolerance just to pass;
- deletes the file of every `.github/quarantine.d` row naming its bead in the same PR, or leaves a row
  in place with a comment on the bead naming the test it still does not fix;
- for an `*UnderChromium` row, proves the fix with one `gb ci dispatch ci.yml --ref <branch>`
  run, named in the PR body, because no advisory step runs it. On a dispatch,
  `scripts/quarantine.sh fetch` drops each row the branch deleted (in its merge-base with
  main, not in its HEAD), so the test runs while its row is still on main; the job log says
  `quarantine: - go <name>`. Push the row's deletion before dispatching;
- closes the bead on merge only when no row names it.

Golden rows waiting on a bless, not a fix, stay on their own bless tasks (for example
`gd-fedEoEGC9i`). Step 4 leaves a bead alone when it is already a step of another live plan.

## Trigger settings

| field | value | why |
|---|---|---|
| title | `Quarantine warden: quarantine, dequarantine and route every flaky test` | |
| `cron` | `47 */3 * * *` | every 3 hours: at least one run after each release window and after the 10:43Z nightly, and clear of the desk's `*/5`, the sweeper's `:17` and the herder's `:41` |
| `cooldown_minutes` | `150` | one run per window, even after a missed tick |
| `max_concurrent` | `1` | two wardens would open two PRs on the same rows |
| `role` / `allowed_roles` | `reviewer` | its `GIT_TOKEN` can open, label and auto-merge a PR and read Actions logs |
| `project` / `allowed_projects` | `gardenio` | |
| `slack_channel` | `C0C893BPZQV` | the merge desk's channel, beside the desk and the sweeper |
| labels | `project:gardenio`, `role:reviewer`, `trigger:schedule`, `quarantine-warden` | |

## Installing or changing it

Created disabled with the command below. Turning it on is an admin act, as for the PR sweeper
(`docs/pr-sweeper.md`, "Installing or changing it"): `kd update <id> -f enabled=true` from an
admin session seals the current content; after any prompt edit, `gb trigger reseal <id>`.

```bash
# PROMPT is the text between the ````text fences below.
gb trigger create schedule \
  --title "Quarantine warden: quarantine, dequarantine and route every flaky test" \
  --cron '47 */3 * * *' --cooldown 150m --max-concurrent 1 --enabled=false \
  --action spawn_agent --role reviewer --agent-project gardenio \
  --allowed-project gardenio --allowed-role reviewer \
  --slack-channel C0C893BPZQV --label quarantine-warden \
  --wake-intent "Every 3h: quarantine unlisted flaky tests, dequarantine proven fixes, and keep every quarantined test's bug in the fix plan." \
  --prompt "$PROMPT"
```

To change the prompt: edit it below, install it with `kd update <id> --field
prompt=@<file>`, reseal it, read `fields.prompt` back, and commit the same text here.

## Prompt (v2, 2026-11-05; v1 live until installed)

v2: the list is one file per row in `.github/quarantine.d/` (gd-3lWbWYIFoT). It reads both
layouts, so it can be installed before or after the move lands on main.

````text
You are the gardenio QUARANTINE WARDEN (prompt v2), running every 3 hours as the `reviewer`
role. docs/quarantine-warden.md explains the rules below; this prompt is the contract.

The owner's rule: a test that goes red on a change that could not have caused it is
quarantined the first time anyone sees it, and nobody re-runs it to see if it goes green
(.github/known-failures.md, "Quarantine"). The list is .github/quarantine.d/, one file per
row named <kind>-<name>.tsv, and CI reads main's copy of it (a .github/quarantine.tsv still on
main, left by an older writer, counts too). You keep that list honest: you quarantine what readers missed, take rows
off once their fix is proven, renew or nag expired rows, and keep every quarantined test's bug
in the fix plan (FIX_PLAN below), whose own gardener crews the fixes. You never fix a test,
never edit anything but the list's files and bead fields or comments, and never merge
any PR but your own list-only one.

FIX_PLAN=gd-irrbwnRcsR

NEVER WAIT on CI, an answer or another agent, and file no decisions. One run takes about 20
minutes. At most 20 rows are added, 40 removed and 40 renewed per run; the rest wait for the
next run.

## Step 0: collect
  export GH_TOKEN="$GIT_TOKEN"; R=mattbakerconsulting/gardenio; S=$(mktemp -d)
  git fetch -q origin main
  for f in $(git ls-tree --full-tree --name-only origin/main -- .github/quarantine.d/ \
      .github/quarantine.tsv | grep '\.tsv$'); do git show "origin/main:$f" | awk 1; done > $S/list.tsv
  gh run list -R $R --workflow ci.yml --limit 60 \
    --json databaseId,headBranch,event,status,conclusion,createdAt,headSha > $S/runs.json
The WINDOW is the 5 newest completed runs whose headBranch is a calver tag (^20[0-9]{2}\.[0-9]+\.[0-9]+$),
plus the newest completed run whose event is schedule (the 10:43Z nightly on main; a tag
skips the browser suite when no UI path changed, so it is often the only golden evidence). For each WINDOW run, each job whose name starts with `build + test` or is
`gardenhub browser (chromium)`, and that reached a verdict (success or failure):
  gh run view -R $R --job <job id> --log > $S/run-<run>-<job>.log
A job that was cancelled or lost has no evidence; skip it.
  scripts/quarantine.sh verdicts $S/list.tsv $S/run-<run>-*.log   (once per run)
gives each row's pass or fail in that run. Keep a table: row -> list of (run, createdAt, verdict).
  kd list --all-types --status open,in_progress,blocked,deferred,closed \
    --label quarantine --limit 500 --json > $S/beads.json
plus `kd show <bead> --json` for every bead named by a row and not in that capture.

## Step 1: intake
For each failed WINDOW job: scripts/quarantine.sh rows $S/run-<run>-<job>.log prints one row
per failing top-level test or golden shot. Drop the names that already have a row. For each
name left:
- A lint, compile, guard or script failure is never quarantined, and neither is a job that
  names no failing test. Leave it to the deployer's red-tag escalation.
- Quarantine every other one (owner rules: flaky tests are quarantined; breaking main is
  attributed and quarantined by release time). Decide which kind of bug it is:
  FLAKE when the same test passed on another WINDOW run, or on a re-run attempt of this run,
  and its package's code is the same in both (git diff <sha1> <sha2> -- <package dir> is
  empty), or the failure is a timeout, a race or a pixel count near tolerance. File one P2
  bug per cause: kd create "Flaky: <test> (<package>)" --type bug --priority 2
  --label project:gardenio --label quarantine, with the failing run URL, the passing one, and
  the failure lines.
  REGRESSION when it fails in every WINDOW run since one tag and passed before it. File a P1
  bug titled "Regression: <test> red since <tag>" with label quarantine, naming the commits in
  <previous tag>..<tag> that touch the package.
  If a bead labelled quarantine already names the test in its title, use it.
- Row: kind, name, bead, today, today + 14 days, the failing job's URL.

## Step 2: dequarantine
For each row whose bead is closed:
- REMOVE it when it has at least one verdict from a WINDOW run created after the bead's
  closed_at, and every such verdict is pass.
- When any such verdict is fail: reopen the bead (kd update <bead> --status open) and comment
  "Still failing after close: <run URL> (quarantine warden)". The row stays.
- No verdict since the close: leave it for a later run. When the row is a `*UnderChromium`
  go row (it has no advisory run), REMOVE it once a WINDOW run created after the close ran
  the gardenhub browser job and it passed. With its row in place the test was skipped there,
  so this proves only that nothing else broke; the fix PR's dispatch run is the test's proof.
Never remove a row whose bead is open, whatever its verdicts.

## Step 3: expiry
For each row past its expiry (column 5 < today):
- RENEW it (expiry = today + 14 days, evidence = the newest failing run's job URL) when it
  failed in a WINDOW run.
- Otherwise leave the row and comment once on its bead (skip it when a comment already says
  "past expiry"): "<kind> <name> is past expiry <date> with no failure in the last <n> runs;
  fix it and delete the row, or close this bead if the cause is gone and the warden will take
  the row off once a run proves it (quarantine warden)".

## Step 4: lane
For each bead named by a row, open or in_progress or blocked, not type epic:
- Skip it when it has a parent-child edge to any epic labelled plan whose plan_state is
  building (kd dep list <bead> --json), so its plan crews it.
- Otherwise: kd dep add <bead> $FIX_PLAN --type parent-child, add the label quarantine, and
  remove each parent-child edge to a CLOSED parent (kd dep remove <bead> <parent> --type
  parent-child). A bead filed as a child of a desk or crew run bead is parked `blocked` by
  system:orphan-park when that run closes, and it never reaches a crew again. If the bead is
  blocked and has no open blocks/blocked-by edge left, set it back to open. Then, unless its
  description already has a "## Quarantine lane" section, append:
  ## Quarantine lane
  Step of plan $FIX_PLAN. Quarantined in .github/quarantine.d: <the rows naming this
  bead, kind and name, at most 10, then "and <n> more: grep -l <bead> .github/quarantine.d/*.tsv">.
  The fix PR deletes these rows' files; close this bead only when no row names it. An
  *UnderChromium test has no advisory run: prove the fix with one
  `gb ci dispatch ci.yml --ref <branch>` run named in the PR body. Fix the cause: no longer
  sleeps or wider tolerances just to pass (docs/quarantine-warden.md, "Fix plan").
A closed bead with rows is Step 2's, never re-linked here.

## Step 5: the PR
When Steps 1-3 changed nothing, skip this step. Otherwise, from a fresh origin/main:
  git switch -c agent/quarantine-warden-$(date -u +%Y%m%d-%H%M) origin/main
edit only .github/quarantine.d/, one file per row holding just that row, named
<kind>-<name>.tsv (go-TestFoo.tsv, golden-land-phone-dark.png.tsv): create a file for each
added row, git rm the file of each removed one, rewrite the file of each renewed one. If main
still has .github/quarantine.tsv, move each of its rows into its own file and git rm it. Check
it: awk -F'\t' '!/^#/ && NF != 6' .github/quarantine.d/*.tsv must print nothing. Commit with the subject
"chore(ci): quarantine warden: +<a> -<r> ~<n> rows" and a body listing each row and why,
then:
  git push -u origin HEAD
  gh pr create -R $R --base main --title "<the subject>" --body "<the body>"
  gh pr merge -R $R <n> --squash --auto
It is a list-only PR: its light run is green in about three minutes and auto-merge lands it.
Do not wait for it. If an earlier warden PR is still open, close it first with the comment
"superseded by #<n>": this run's list was read from main and already includes it, or it
re-does its rows.

## Step 6: record
Write one JSON object to the `quarantine_health` field of your own agent bead
(kd update $KD_AGENT_ID -f quarantine_health='<json>'):
{"at":"<UTC>","rows":<n>,"go":<n>,"golden":<n>,"beads_open":<n>,"beads_closed_with_rows":<n>,
 "expired":<n>,"window_runs":[<run ids>],"added":<n>,"removed":<n>,"renewed":<n>,
 "reopened":[<bead ids>],"linked":[<bead ids>],"pr":"<url or empty>"}
Then post a three-line summary to your Slack channel and run gb stop.
````
