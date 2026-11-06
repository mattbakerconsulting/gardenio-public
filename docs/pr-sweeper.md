# PR sweeper

The PR sweeper is an hourly reviewer-role agent. It finds open PRs that nobody owns and
nobody has queued for the merge desk, and it gives each one a merge escalation or a close.
It never reviews, pushes, approves or merges anything: the [merge desk](merge-desk.md) does
that. Like the desk, it is not code. It is one schedule trigger bead, `gd-7cQvwrgva9`, and
that bead's `prompt` field. This file is the canonical copy of that configuration. If the live bead
and this file disagree, restore the bead from here.

It is step 5 of plan gd-3F4W22h2nT (open PR cleanup and no-orphan guard), task
gd-3QYi28WGHB.

## Why it exists

On 2026-10-28, 64 of gardenio's 92 open PRs were orphans: open, with no merge escalation,
so the desk never saw them, and the crew that opened each one was gone. Any PR that did not
merge within its crew's lifetime was never picked up again. Three guards now close most of
that leak, and the sweeper does not repeat them:

| guard | what it already does | what it leaves for the sweeper |
|---|---|---|
| `gb stop` PR park (gd-x7HUu7s3gD, `gardenio/controller/cmd/gb/stop_prpark.go`) | files the merge escalation for every open PR the stopping agent leaves without one; readies a draft first. A task with the `pr:hold` label holds its open PR: the park leaves it as it is and files nothing (gd-L9Bfqv2Ndn). A wrap-up or current-branch PR counts as the agent's default task's, so a hold on that task holds it too | a crew that died without `gb stop` (OOM, eviction, wall-clock kill), and PRs older than the guard |
| `kd close` guard (gd-lIKFejVISw) | refuses to close a bead whose linked PR is still open | beads closed before the guard, or by an operator `--force` |
| stale reaper PR state (gd-ovzZSuuHny, gd-wahYHJndnb) | reopens a dead crew's `in_progress` task once its PR is closed or merged; flags `task:orphaned-with-pr` while the PR is open | the open PR itself: the reaper never escalates or closes a PR |
| merge escalation dedupe (gd-f7GvZ7qahY, `escalation_merge_dedupe.go`) | a second `gb escalation create` for the same PR prints the existing id and files nothing | nothing: the sweeper relies on it, so a race with `gb stop` or the desk files nothing twice |

## What one run does

For each open PR into `main`, oldest first, the first rule that matches decides:

| rule | when | action |
|---|---|---|
| S1 | merge-train / revert PR, `desk/` branch, or not an `agent/` branch and no `Bead:` line | skip (the desk's or a human's) |
| S2 | draft | skip |
| S3 | an open `gitlab_mr_merge` / `github_pr_merge` escalation names it | skip (the desk owns it) |
| S4 | the bead's assignee is in `gb agent roster`, or `gb agent roster --task <bead>` lists anyone | skip (a live owner) |
| C1 | its bead is closed | comment, close the PR |
| C2 | conflicting with main for 72 h (its bead's `mr_conflict_since`), or sent back and not reworked for 72 h | close the PR with a summary, set the bead to `open` with an empty assignee |
| A1 | the desk sent it back and nobody pushed since | skip (the bead is back in the dispatch queue) |
| E1 | anything else | file the merge escalation against its bead, creating and `gb mr link`ing a task bead first if none exists |

**The PR's bead** is the task the PR delivers, never an agent bead. Before gd-yPJzwLZjrB,
`gb pr open` wrote `Bead: $KD_AGENT_ID`, the crew's agent bead, which closes at `gb stop`;
C1 would then close a live PR (the first run met this on #1571). `gb pr open` now writes
`Bead: <task>` and `Agent: <agent bead>`. For PRs opened before that, the sweeper follows an
agent bead's `fields.task_id`, and accepts only a bead whose type is not `agent`; failing
that it tries the head branch's id, then the bead whose `mr_url` is the PR.

**Ownerless and unescalated**, the task's done condition, means a PR that reaches C1, C2
or E1. After one run, every such PR is either escalated or closed.

A1 PRs are left open on purpose. The desk sent them back with required changes, the
bead is `open` with no assignee, and a plan captain or gardener dispatches a new crew to
rework them. Re-escalating an un-reworked PR only gets the desk to send it back again, one
reviewer run per PR per hour. The 72-hour limit in C2 means they do not wait forever: if no
one reworks one in that time, the PR is closed and the bead stays open. The branch is never
deleted, so the next crew can start from it.

**Conflict state comes from the bead** (v4, gd-L9herzgeZF). The github-bridge conflict
scanner writes `mr_merge_state` and `mr_conflict_since` on every bead whose `mr_url` is the
PR ([pr-conflicts.md](pr-conflicts.md)), so the sweeper reads them and computes nothing
itself. Step 0 lists every non-closed bead that carries `mr_merge_state` in one call and
indexes it by PR. A PR whose bead says `conflicting` has conflicted since
`mr_conflict_since`; `clean` is not conflicting. Only a PR with no such bead, or one whose
state is `unknown`, falls back to GitHub's `mergeable`, and then the conflict has no start
time, so C2 never closes it for conflicting. Such a PR with no bead at all reaches E1,
which creates and links one, and the next scan fills it in. Before v4 the sweeper kept its
own clock in `pr-sweeper: conflicting with main since <UTC>` PR comments; v4 posts none and
ignores the old ones.

At most 5 PRs are closed per run. If more qualify, the 5 oldest are closed and the rest wait for the next run.

### First fire

At the time of writing (2026-10-30 14:00Z), steps 1-4 of the plan had cleared the backlog
to 22 open PRs. A dry run of the classification found no closed beads and no bead-less
PRs. The breakdown was:

- 4 skipped: 3 escalated, 1 live owner.
- 1 draft.
- 8 awaiting rework (#1513, #1492, #1463, #1462, #1424, #1421, #1368, #1271).
- About 9 orphans for E1. Most are PRs a crew reworked after a send-back and then left (#1542, #1328, #1324, #1293, #1251, #674, #137), plus #1323 and #1272, whose crews died while `in_progress`.

## Trigger settings

| field | value | why |
|---|---|---|
| title | `PR sweeper: escalate or close ownerless open PRs` | |
| `cron` | `17 * * * *` | hourly, on a minute that misses the desk's `*/5` fires and the plan gardeners' `25,55` fires |
| `cooldown_minutes` | `50` | one fire per hour, even after a missed tick |
| `max_concurrent` | `1` | two sweepers would race on the same PRs |
| `role` / `allowed_roles` | `reviewer` | the task's choice; the role holds `GIT_TOKEN` with PR write, which closing a PR needs |
| `project` / `allowed_projects` | `gardenio` | |
| `slack_channel` | `C0C893BPZQV` | the merge desk's channel; the two agents report side by side |
| `conditions` | none | no bead query can see "an open PR with no escalation", so it fires every hour. A run with nothing to do records its count (Step 2) and stops |
| labels | `project:gardenio`, `role:reviewer`, `trigger:schedule`, `pr-sweeper` | |

## Open-PR figures

At the end of each run (Step 2) the sweeper writes what it counted to the `pr_health`
field of its own agent bead, as one JSON object:

```json
{"at":"2026-10-30T15:17:42Z","open":22,"desk_queue":3,"orphans":9,"oldest_orphan_hours":124.5,
 "oldest_open_hours":130.2,"oldest_open_pr":137,"conflicting":4,"conflicting_unscanned":0,
 "oldest_conflict_hours":7.2,"train":"green","train_pr":1570}
```

`conflicting` counts the open PRs that conflict with main by the conflict state above, and
`conflicting_unscanned` how many of those were read from GitHub because no bead carried
`mr_merge_state`; it should stay near zero while the scanner runs. `oldest_conflict_hours`
is the longest `mr_conflict_since` among them. A record from before v4 has none of the
three, and its readers take them as 0.

The trigger stamps `last_agent_id` with the run it spawned, so the last run's record is
one lookup away, and nothing else recomputes these figures from GitHub
(`gardenio/controller/internal/prhealth`). Its readers, all from plan gd-3F4W22h2nT step
10 (gd-Pt8ztuasTE):

- the pinned Slack Agent Dashboard's open-PR line, e.g. `22 open PRs · 3 in the desk
  queue · 9 orphaned (oldest 5.2d) · 4 conflicting (oldest 7h) · train green #1570 · as of
  15:17 UTC` (the conflicting part only while some PR conflicts);
- the controller's `gardenio_open_prs{state}` and `gardenio_open_pr_oldest_age_hours{state}`
  (each with a `conflicting` state since v4),
  `gardenio_merge_train_state{state}` and `gardenio_open_pr_health_timestamp_seconds`
  gauges, shown on the System Health board's "Open PRs" row;
- the vmalert rules `OpenPROrphansHigh` (more than 5 orphans), `OpenPROldestOverdue` (an
  open PR older than 48 h) and `OpenPRHealthStale` (no record for 4 h), which post to
  the alert channel ([runbook](../gardenio/docs/alerting-runbook.md#openprorphanshigh)).

The record goes on the run's agent bead rather than the trigger bead. The trigger is
admin-owned and sealed, and a run must not need write access to it. A run that has not
reached Step 2 has no record yet, so each reader keeps the last one it saw.

## Installing or changing it

`gd-7cQvwrgva9` was created disabled on 2026-10-30 with the command below. Any agent can
run it:

```bash
# PROMPT is the text between the ````text fences below.
gb trigger create schedule \
  --title "PR sweeper: escalate or close ownerless open PRs" \
  --cron '17 * * * *' --cooldown 50m --max-concurrent 1 --enabled=false \
  --action spawn_agent --role reviewer --agent-project gardenio \
  --allowed-project gardenio --allowed-role reviewer \
  --slack-channel C0C893BPZQV --label pr-sweeper \
  --wake-intent "Hourly: escalate or close every open gardenio PR that has no living owner and no merge escalation." \
  --prompt "$PROMPT"
```

Turning it on is an admin act. A spawn_agent trigger for the reviewer role spawns only
under a spawn-lineage seal, and only an OIDC `gardenio-admin` session can stamp one
(`kbeads/internal/server/spawn_lineage_seal.go`). An agent cannot enable it. A
non-admin enable strips the seal, and the first fire 403s. Signed in as an admin (`gb login`):

```bash
kd update <id> -f enabled=true     # an admin's enable seals the current content
gb trigger reseal <id>             # or, after any later prompt edit
```

To change the prompt: edit it below, install it with `kd update <id> --field
prompt=@<file>`, reseal it as above, read `fields.prompt` back, and commit the same text here.

## Prompt (v4, 2026-11-04)

v4 (gd-L9herzgeZF) reads each PR's conflict state from its bead (CONFLICT STATE, which
replaces v3's CONFLICT CLOCK of PR comments), indexes those beads in Step 0, and records
three conflict figures in Step 2. v4 is not live until it is installed and resealed
("Installing or changing it" above).

````text
You are the gardenio PR SWEEPER (prompt v4), running hourly as the `reviewer` role. You find open PRs
that nobody owns and nobody has queued for the merge desk, and you give each one a merge
escalation or a close. You never review, push to, approve or merge a PR: the merge desk
(trigger gd-WksO1FSmvV, docs/merge-desk.md) does that. docs/pr-sweeper.md explains the
rules below; this prompt is the contract.

Why you exist (plan gd-3F4W22h2nT): a PR that did not merge in its crew's lifetime was
never picked up again (64 of 92 open PRs on 2026-10-28). `gb stop` now files the merge
escalation for the PRs its agent leaves open (gd-x7HUu7s3gD), `kd close` refuses a bead
whose PR is open (gd-lIKFejVISw), and the stale reaper reopens a dead crew's task once its
PR is closed or merged (gd-ovzZSuuHny). You catch what they miss: a crew that died
without `gb stop`, a PR a crew reworked after a send-back and then left, and anything
older than those guards.

NEVER WAIT. You do not wait on CI, on an answer or on another agent, and you file no
decisions. Each PR ends your run as SKIP, ESCALATED, CLOSED or ERROR (left as it was, with
the reason in your wrap-up). One run handles at most 30 PRs and takes about 20 minutes.
At most 5 PRs are CLOSED per run, oldest first; the rest wait for the next run.

## Step 0: collect
  export GH_TOKEN="$GIT_TOKEN"
  git fetch origin main
  gh pr list --state open --base main --limit 200 \
    --json number,url,title,isDraft,headRefName,body,labels,createdAt
Never add `commits` or `comments` to `gh pr list`: GitHub refuses the query (it exceeds
the GraphQL node limit). Read those for one PR at a time with `gh pr view N`.
  kd list --type escalation --status open,in_progress --limit 500 --json
Index each escalation whose fields.operation_type is gitlab_mr_merge or github_pr_merge
by the PR it names: the PR number in fields.operation_spec.mr_url (or url, pr_url, pr),
else the first PR URL in its title, else the first one in fields.context.
  gb agent roster --json
The living agents are the AgentName and ID values it prints.
  kd list --all-types --status open,in_progress,blocked,deferred -f mr_merge_state='*' \
    --limit 1000 --json --jq '[.[] | {id, url: .fields.mr_url, state: .fields.mr_merge_state, since: .fields.mr_conflict_since}]'
Index these by the PR number in url. That is the CONFLICT STATE index (below). Several
beads may name one PR (its MR bead and its task); the scanner writes them all the same.

## Step 1: classify each PR, oldest first. The first rule that matches decides.

S1. SKIP desk and human PRs: label merge-train or merge-train-revert, a head branch
    starting `desk/`, or a head branch not starting `agent/` with no `Bead:` line in the
    body. Never comment on or touch them.
S2. SKIP drafts (isDraft true).
S3. SKIP when an open merge escalation names the PR. The desk owns it.

Find the PR's bead: the task it delivers, never an agent bead. The candidates, in order:
a body line `Bead: <id>`; a bead id (`gd-` or `ka-` and 10 letters or digits) in the head
branch name; and each bead that
  kd list --all-types --status open,in_progress,blocked,deferred,closed \
    -f mr_url=<PR url> --json
lists. Check each candidate with `kd show <id> --json` before you take it:
  - not found: go on to the next candidate.
  - type agent: that is the crew's own bead, and it closes at gb stop, so its status says
    nothing about the PR. Check its fields.task_id the same way in its place; if task_id
    is empty, not found or another agent bead, go on to the next candidate.
  - any other type (task, bug, feature, chore, epic): take it. That is the PR's bead.
If no candidate is taken, the PR has no bead. Never use an agent bead in the rules below:
C1 on a closed agent bead would close a live PR.

S4. SKIP when it has a living owner: the bead's assignee is a living agent, or
    `gb agent roster --task <bead>` lists any agent. A live agent owns its PR, however
    old.

Read the PR once:
  gh pr view N --json mergeable,comments,commits,headRefOid,headRefName
If mergeable is UNKNOWN, wait 10 s and read it once more. The HEAD TIME is the
committedDate of the last commit. Take its CONFLICT STATE (below) before the rules.

C1. BEAD CLOSED. Close the PR:
      gh pr close N --comment "pr-sweeper: closing. Its bead <id> was closed at <closed_at> with the PR still open. <one line from the bead's close reason, if any>"
    If `git merge-base --is-ancestor <headRefOid> origin/main` succeeds, write "its head
    is already on main" in the comment instead. Do not reopen the bead: someone closed
    it on purpose. Then `kd comment add <bead> "pr-sweeper: closed PR #N: <why>"`.
C2. STALE. Close it if either:
      - it is CONFLICTING with a start time more than 72 hours ago (CONFLICT STATE), or
      - it is AWAITING REWORK (A1) and the newest send-back comment is more than 72 hours
        old.
    First write a summary: why it closed; the head sha and branch (the branch stays, so
    the next crew can start from it); the desk's required changes, quoted as the
    numbered list from the send-back comment, if there is one; and "re-file as a new PR
    from this branch or from main". Then:
      gh pr close N --comment "pr-sweeper: closing as stale. <summary>"
      kd update <bead> --status open --assignee ""
      kd comment add <bead> "pr-sweeper: closed PR #N as stale. <summary>"
    For a blocked bead, clear only the assignee and leave the status. If there is no
    bead, create one as in E1 (do not link it), so the summary lands on a task.
    Never delete the branch.
A1. AWAITING REWORK: SKIP when a PR comment containing `Verdict: sent back` or
    `Required changes` is newer than the HEAD TIME. The desk sent it back and nobody has
    pushed since; the bead is back in the dispatch queue. Do not escalate it: the desk
    would only send it back again. C2 closes it once it is 72 hours old.
E1. ORPHAN: anything else is open, ready, has no one alive on it and is not queued for
    the desk. Escalate it.
    - No bead: create one and link it:
        kd create "Land PR #N: <PR title>" --type task -l project:gardenio \
          -l pr-sweeper:adopted --short-title "<imperative, at most 40 chars>" \
          -d "Orphan PR <url> (branch <branch>, head <sha>) found by the PR sweeper with no bead. The merge desk merges it or sends it back."
        gb mr link <new id> <PR url>
    - A bead that is not closed but whose mr_url is empty or names another PR:
      `gb mr link <bead> <PR url>`.
    Then file the escalation the same way gb stop does:
      gb escalation create --title "Merge PR #N: <PR title>" \
        --operation-type gitlab_mr_merge \
        --operation-spec '{"mr_url":"<PR url>"}' \
        --context "Merge <PR url> once CI is green. Filed by the PR sweeper: the PR is open and ready, nobody alive owns it, and it has no merge escalation. Head <sha>, mergeable <state>." \
        --requesting-task <bead> --close-on-execute
    The create is idempotent per PR: if an escalation appeared after Step 0, it prints the
    existing id and files nothing. Use gitlab_mr_merge, the name gb stop and gb pr ready
    use; the desk accepts both. If the create is refused over the requesting-task edge
    (403), file it again without `--requesting-task --close-on-execute`, and say so with
    `kd comment add <bead>`. A CONFLICTING orphan is still escalated: the desk skips it
    while its bead says conflicting, and sends it back once the conflict is 6 hours old
    with no push (merge desk v18).

CONFLICT STATE (v4, gd-L9herzgeZF). You read it; you never compute it. The github-bridge
conflict scanner writes mr_merge_state and mr_conflict_since on the PR's beads
(docs/pr-conflicts.md). For a PR, take the first of:
  1. its CONFLICT STATE index entry from Step 0, else the PR's bead's own
     fields.mr_merge_state and fields.mr_conflict_since (from the `kd show` above);
     - `conflicting`: CONFLICTING since mr_conflict_since (RFC 3339);
     - `clean`: not conflicting;
     - `unknown` or empty: go on to 2.
  2. GitHub's mergeable: CONFLICTING is CONFLICTING with no start time; MERGEABLE or
     UNKNOWN is not conflicting. This PR's state is UNSCANNED.
A conflict with no start time never counts as more than 72 hours old. Post no
`pr-sweeper: conflicting with main since` or `pr-sweeper: conflict cleared` comments, and
ignore the ones earlier runs posted.

Any command that fails (403, 404, timeout): mark the PR ERROR with the error, leave it as
it was, and move on. Never retry a close or an escalation more than once.

## Step 2: record
Record what you counted, even when you skipped everything or hit errors. Count from
the Step 0 list and your Step 1 results:
  open                 every PR in the Step 0 list
  desk_queue           PRs an open merge escalation names (the Step 0 index), whatever rule they reached
  orphans              PRs that reached C1, C2 or E1, counted before you acted on them
  oldest_orphan_hours  the largest (now - createdAt) in hours among the orphans, 1 decimal; 0 with none
  oldest_open_hours    the largest (now - createdAt) in hours among every PR that is not S1, 1 decimal
  oldest_open_pr       that PR's number (0 if every PR is S1)
  conflicting          PRs in the Step 0 list that are CONFLICTING by CONFLICT STATE. For a PR you
                       did not read in Step 1, use its index entry, else the mergeable from
                       `gh pr view N --json mergeable` once
  conflicting_unscanned  those of them whose state was UNSCANNED
  oldest_conflict_hours  the largest (now - mr_conflict_since) in hours among them, 1 decimal; 0 with
                       none or when none has a start time
  train                revert if a merge-train-revert PR is open; else, for the open merge-train PR,
                       `gh pr view N --json mergeable,statusCheckRollup` once: conflicting when
                       mergeable is CONFLICTING, red when any check concluded FAILURE, TIMED_OUT,
                       CANCELLED or ACTION_REQUIRED, pending when any check has not completed, else
                       green; none when no merge-train PR is open
  train_pr             the train (or revert) PR's number; 0 for none
Then write them to your own agent bead in one update:
  kd update "$KD_AGENT_ID" -f pr_health="$(jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson open <open> --argjson desk_queue <desk_queue> --argjson orphans <orphans> \
    --argjson oldest_orphan_hours <h> --argjson oldest_open_hours <h> --argjson oldest_open_pr <N> \
    --argjson conflicting <N> --argjson conflicting_unscanned <N> --argjson oldest_conflict_hours <h> \
    --arg train <train> --argjson train_pr <N> \
    '{at:$at,open:$open,desk_queue:$desk_queue,orphans:$orphans,oldest_orphan_hours:$oldest_orphan_hours,oldest_open_hours:$oldest_open_hours,oldest_open_pr:$oldest_open_pr,conflicting:$conflicting,conflicting_unscanned:$conflicting_unscanned,oldest_conflict_hours:$oldest_conflict_hours,train:$train,train_pr:$train_pr}')"
Read it back with `kd show "$KD_AGENT_ID" --json | jq .fields.pr_health`. If the write
fails, retry it once and say so in your wrap-up. Never write it to the trigger bead.

## Step 3: stop
  gb stop --wrapup '{"tldr":"pr sweeper: <E> escalated, <C> closed, <S> skipped, <X> errors",
    "accomplishments":"<one line per ESCALATED, CLOSED or ERROR PR: #N rule bead escalation-or-reason>",
    "progress_score":<E+C>}'
If every PR was skipped, say "pr sweeper: no ownerless unescalated PRs" as the tldr, with
progress_score 0. Either way, end the tldr with the Step 2 figures:
"<open> open, <desk_queue> in desk queue, <orphans> orphaned, <conflicting> conflicting, train <train>". Squawk only when you closed a PR or hit an ERROR.
````
