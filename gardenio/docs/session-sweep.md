# Session sweep: find slow and bad agent-session patterns

`gb session sweep` reads the last day of session and agent beads, plus a replay of the
HOOK_EVENTS bus, and lists the agents that are wasting pods, tokens or operator attention.
It takes under ten seconds against the live fleet (about 7s for ~860 sessions and ~1,350 agent
beads on 2026-10-30). Any agent can run it with its own token. It reads only, unless you pass
`--file`.

It was built for plan gd-xV14ol0GhR (task gd-S6ce5ycn45). The founding case was
crew-gardenio-infra-5juu: an agent whose own token got a 403 on its bead, so it re-ran a
finished task every ~10 minutes for hours.

```sh
gb session sweep                          # report, last 24h
gb session sweep --json | jq '.counts'    # machine-readable
gb session sweep --since 6h --slow 20m    # narrower window, higher slow bar
gb session sweep --file --parent gd-xV14ol0GhR   # the gardener's per-cycle run
```

The [session survey](session-sweep-survey.md) (2026-10-30, 251 sampled sessions) lists the
problem classes found in archived sessions. For each class it gives the signal, how often it
appeared, what it cost and whether the sweep catches it. It also measures each detector's
false-positive rate against the sample.

## Patterns

| Pattern | Flags | Source | Default threshold |
|---|---|---|---|
| `restart-loop` | an agent bead with more than N sessions starting inside any one hour (per agent bead, not agent name: a gardener's or nursery's decision, escalation and schedule wakes each get a fresh agent bead, and were the false positives) | session beads `agent_bead_id`, `started_at`, `restart_cause` | `--restart-max 3` |
| `auth-storm` | N or more kd/gb calls in one session answered `HTTP 401:`/`HTTP 403:` that name the agent's own bead or task, or run a gb verb that acts on its own bead (stop, yield, prime, decision, escalation, squawk, gate, wrapup) | HOOK_EVENTS `PostToolUse`/`PostToolUseFailure` | `--auth-min 3` |
| `oom` | a session `exit_code` 137 or an OOM `restart_cause` (the controller's status reporter stamps both on the session an `OOMKilled` agent container was running), or an agent `last_coop_exit_code` 137, or a `retired` session with no exit code whose container restarted under it (a `resumed` session on the same agent bead and host started before it ended) when the agent's wrap-up names an OOM or the memory limit | session and agent beads | none |
| `repeat-call` | the same tool call (tool + input, ignoring Bash's description) N or more times in one session | HOOK_EVENTS | `--repeat-min 5` |
| `dup-comment` | one author posting the same whitespace-normalised text twice or more on one task bead | comments on each agent's `task_id` | 2 |
| `slow-call` | a tool call at or over the bar, sorted into kinds: `ci-wait`, `wait-on-process` (a foreground `until`/`while … sleep`, `pgrep` or `tail --pid` loop, checked before the job it waits on), `goldens` (including `make commons-ux-shoot`), `build-test` (including `golangci-lint`), `other` | session beads `slowest_calls`, and HOOK_EVENTS `duration_ms` for the retired sessions that have no `slowest_calls` (one call counted once, by `tool_use_id`) | `--slow 10m`; `--wait-slow 5m` for `wait-on-process` |
| `harness-gap` | a wrap-up whose `harness_gaps` names a missing tool, command, subcommand or binary (not a missing permission: a sentence naming a 403, a denial or a permission is skipped), grouped by subject such as `gb pr` or `gofumpt` | agent beads `wrapup` | none |
| `gh-token-scope` | the fleet GitHub token refused (`Resource not accessible by personal access token`): kind `tool` per session from the bus; kind `wrap-up` for a `harness_gaps` sentence naming the token, a CI read or re-run, and a refusal. Fixing it needs wider access, routed to the operator (gd-ZVdKWcx2xD) | HOOK_EVENTS, agent beads `wrapup` | 1 |
| `ownership-403` | kd/gb writes to the agent's own `task_id` answered `HTTP 403: ownership denied` (a refusal on some other bead is not flagged); for a thread agent, which has no `task_id`, any refusal that names the agent as the caller (`you are "<agent>"`) | HOOK_EVENTS, agent `task_id`/`thread_id` | 1 |
| `infra-access-gap` | `gardenio-infra-reader" cannot <verb> resource "<r>"`, kind = the resource; `secrets` is a correct denial and is skipped. Wider access is routed to the operator (gd-3Xa9PZgybq) | HOOK_EVENTS | 1 |
| `browser-deps-eacces` | `EACCES: permission denied, mkdir '/ms-playwright…'` from the browser install | HOOK_EVENTS | 1 |
| `pkill-self-match` | a `pkill -f` that fails `Exit code 144`: the pattern matched the agent's own shell | HOOK_EVENTS | 1 |
| `tmpdir-unset` | a command writing `$TMPDIR/…` refused at the filesystem root (`/esc.json: Permission denied`): `TMPDIR` is unset | HOOK_EVENTS | 1 |
| `wrapup-quoting` | `gb stop --wrapup '…'` failing `syntax error near unexpected token` or `unexpected EOF while looking for matching` | HOOK_EVENTS | 1 |
| `stop-refused-schedule` | `gb stop` refused with `you have claimed in-progress work` on a `Schedule:` bead (a scheduled run's own schedule claim) | HOOK_EVENTS | 1 |
| `retrigger-churn` | 3 or more `git push`es in one session that each carry a merge of `origin/main` or a `git commit --allow-empty`, to re-run CI or chase main. Only the push counts: a trial merge (`--no-commit`, then `--abort`) that never leaves the pod and `git merge-base` reads do not (gd-EhoChYt9st); a merge-desk CONFLICT MERGE counts as one | HOOK_EVENTS | 3 |
| `bead-reread` | `kd show` of one bead id 4 or more times in one session (keyed on the id, so varying pipes still count) | HOOK_EVENTS | 4 |
| `stop-did-not-take` | a completed or crashed session (not retired: retired `ended_at` trails by the ~16 min retire lag) that ended 30 min or more after its last tool call; count is the idle minutes. With no bus trace (an own-bead 403 refuses the hook emitter too), a `gb stop` in the session's `slowest_calls` is the last call, if the agent bead stored no wrap-up inside the session | session beads (`session_log` names the bus `session_id`; `slowest_calls`), agent beads `wrapup`, HOOK_EVENTS | 30 min |
| `killed-mid-task` | a session ended by `credential_switch` (or a crash after 5 min, or a pod terminate: restart cause `sigterm` with the agent bead closed `context_cancelled_involuntary` within a minute of the session's end) that made tool calls, never ran `gb stop`, and stored no wrap-up inside it; kind is the restart cause | session and agent beads, HOOK_EVENTS | none |
| `interrupt-then-crash` | a user interrupt in a session that exits 143 within 3 min. The interrupt is a rejected or interrupted tool call on the bus (`The user doesn't want to proceed with this tool use`), or the session bead's `last_interrupt_at`, which `gb` counts from the transcript at close (no text kept), so an interrupt between turns counts too. A bead that ended before gb counted interrupts (`sweepInterruptCountedSince`, 2026-10-30 11:00Z; `interrupt_count` is left out when 0, so its absence marks nothing) counts on shape alone: exit 143, restart cause `crash`, under 3 min, and at least one tool call, which leaves out 4-8 s startup crashes. A requested teardown is skipped: coop's shutdown drain and the agent pod's preStop hook press Escape, which Claude records as a user interrupt, so a session with restart cause `sigterm`, or one whose agent bead closed `context_cancelled…` within a minute of its end (a pod terminate an older gb labelled `crash`), is not this class (gd-Ga8iHjLaYR) | session beads, HOOK_EVENTS | 3 min |
| `noop-followthrough` | an agent woken for an escalation or decision (`wake_reason`) whose wrap-up `tldr` says there was nothing left to do | agent beads `wake_reason`, `wrapup` | none |
| `dispatch-collision` | an agent whose wrap-up `tldr` says a PR had already done its work, with the PR next to the phrase (`already landed on PR #1246`, `#1204 was already merged`), when a different agent's earlier wrap-up lists that PR in `pull_requests`; kind `pr`. Escalation and decision wakes are skipped, as is the PR's own agent coming back and a triggered run (the merge desk, a scheduled or fire-now gardener: `spawned_by trigger:<id>`, `trigger_id`, `schedule_id`, or `spawn_source` `schedule`/`fire-now`), which sweeps for its own work | agent beads `wrapup`, `wake_reason`, `spawned_by`, `trigger_id`, `schedule_id`, `spawn_source` | 1 |

`gh-token-scope` (bus), `infra-access-gap` and `browser-deps-eacces` match the response
text alone, so they skip a call that only reads or searches: a Read, Grep or Glob call, or a
Bash command each pipeline segment of which leads with `grep`, `rg`, `git grep`/`log`/`show`,
`cat`, `sed` (without `-i`), `head`, `tail` or the like, with no `$(…)` and no redirect to a
file. Such a call is usually echoing a docs page or Makefile comment that quotes the error
(gd-puMsI5w1kN). A command that runs anything else (`npx playwright`, `kubectl`, `make`)
still matches.

## Filing (`--file`)

With `--file`, each pattern that has findings gets **at most one open bug**, labelled
`session-sweep` and `session-sweep:<pattern>`:

- If no open bug in the project (`project:$BOAT_PROJECT`, or `--project`) carries the label,
  the sweep creates one (P2, `project:$BOAT_PROJECT`)
  with the evidence: agent, agent bead, session bead ids, counts and a sample. With
  `--parent`, it links the bug under that bead as parent-child.
- If an open bug exists, the sweep comments **only the evidence it has not seen before**.
  The bug's description and each sweep comment end in a `sweep-keys:` line holding a
  short hash per finding (at most 400 per line), so running every gardener cycle does not
  repeat itself. Bug beads have a fixed field schema, which is why the keys live in text.
- The hash covers only a finding's stable identity, never a count or the detail text:

  | Pattern | Identity |
  |---|---|
  | `restart-loop` | the agent and its agent bead (the busiest hour moves as old sessions age out) |
  | `auth-storm` | the session |
  | `oom` | the session, or the agent bead when no session bead shows it |
  | `repeat-call` | the session, the tool and its input |
  | `dup-comment` | the author, the task and the comment text |
  | `slow-call` | the session and the `slowest_calls` entry (tool, time, args), or for a bus call the session, tool and `tool_use_id` |
  | `harness-gap` | the agent bead and the sentence |
  | `gh-token-scope`, `infra-access-gap`, `browser-deps-eacces`, `pkill-self-match`, `tmpdir-unset`, `wrapup-quoting`, `stop-refused-schedule`, `retrigger-churn` | the session and the kind (a `wrap-up` gh-token-scope: the agent bead) |
  | `ownership-403` | the session |
  | `bead-reread` | the session and the bead |
  | `stop-did-not-take`, `killed-mid-task`, `interrupt-then-crash` | the session |
  | `noop-followthrough` | the agent bead |
  | `dispatch-collision` | the agent bead and the PR |

  So a count that moves between sweeps (a loop going from 4 to 9 sessions, a lossy bus
  replay reading "called 7 times" then "called 9 times") is not new evidence.
- With no open bug, the sweep also reads the `sweep-keys:` of the pattern's bugs closed
  inside the window and drops the evidence they already carry. So closing a bug (fixed,
  stale, or judged a false positive) does not bring the same evidence back as a fresh bug
  while it stays in the window; if nothing is new, the pattern reports `unchanged` naming
  the closed bug. Evidence with a new key, such as a recurrence after a fix, still files a
  fresh bug (gd-puMsI5w1kN).

## Limits and known false positives

- **The HOOK_EVENTS replay is partial.** The bus keeps about five hours, and the replay
  drops events. The daemon's `/v1/bus/events` handler pushes replayed NATS messages into a
  128-slot channel and drops on full, and a long replay lost about half its events on
  2026-10-30. So `auth-storm` and `repeat-call` counts are lower bounds, and a short storm
  can be missed. The report prints the window the replay actually covered (`bus … to …`).
  The fix is filed under the plan.
- **The hook relay truncates long calls.** `gb hook relay` cuts `tool_input` and
  `tool_response` to 1024 bytes before they reach the bus. A Bash command longer than that
  arrives as a plain string, so `auth-storm` cannot read the command. A cut response keeps
  its error and refusal lines and its last line in `tool_response_excerpt` (at most 1024
  bytes; not for file-content tools such as Read and Grep), so a denial printed after the
  cut is still seen unless the excerpt's budget runs out first. `auth-storm` counts are a
  lower bound for long commands too. `repeat-call` still counts them, keyed on the truncated input.
- **`slow-call` includes deliberate waits.** Most `ci-wait` entries are a crew polling `gh run`
  for up to an hour in one call. That is the pattern worth fixing (the pod is held and the turn
  is blocked), but it is not a bug in the command. A kind is assigned by the first regex that
  matches, so a chained command (`git commit … && gh run watch`) lands in `ci-wait`, and a commit
  whose message says "goldens" lands in `goldens`.
- **`dup-comment` reads only task beads named by an agent's `task_id`.** Beads in another
  project's scope, or reads refused with HTTP 429 after one retry, are skipped and counted in
  a warning. System actors (`machine:…`) are reported too: the first run caught the viewer
  re-posting the same "approve not applied yet" notice on every retry.
- **`restart-loop` counts session beads, not pods, and only those after an unclean end.** A
  thread agent that resumes for each Slack reply can reach eight sessions an hour in a busy
  thread (gd-fKRCOO3O8h). A session with no `restart_cause` whose predecessor on the same agent
  bead ended on a `gb stop` or `gb yield` that succeeded on the bus (not a failure, no 401/403)
  is not counted. Session beads carry no per-session hook id, so the stop is matched by agent
  bead and time (between the two sessions' starts). A session with a `restart_cause` always
  counts. When the predecessor left no `gb stop`/`gb yield` on the bus, the session counts only
  if the predecessor started at or after the replay's first event (its 403 refused the hook
  emitter too, the 5juu loop). A predecessor that started before the replay is unknown and is
  not counted: a 24h sweep replays only about 5h, and parked crews woken several times an hour
  (`ci_wait_parked`, escalation wakes) read as loops there (gd-QZe4Dmf8a4). With an empty
  replay (`--no-bus`, or it failed) there is no window and every missing stop counts.
- **`harness-gap` is regex-matched prose.** It ignores missing permissions ("the PAT cannot
  rerun jobs"). Those are access asks, which go to the operator as escalations, not bugs.
- **`slow-call` from the bus covers only the replay window.** A retired session's long
  call is seen only while the bus still holds it; `slowest_calls` covers the rest.
- **`dispatch-collision` reads prose, and only inside the window.** The PR's other agent must
  have a wrap-up among the agent beads the sweep fetched (updated in `--since`), so a crew sent
  to beads a week-old PR fixed is missed. A plan gardener closing already-fixed beads names no
  PR next to the phrase and is not flagged; the collision was its earlier crews'.
- **`noop-followthrough` reads prose.** It needs both the wake (`wake_reason` escalation or
  decision) and a `tldr` saying "nothing left to do"; most escalation-resume wakes end as
  no-ops, so it is common by design: the fix is not to wake for a closed task.
- **`stop-gate-wait` needs the gate's Stop events.** Only a pod whose `gb` publishes them
  (gd-fSdUbOmf3g) can be flagged. It counts Bash calls that poll CI or wait on a process
  (`gh run`, `sleep`, `tail --pid`, ...) starting within 2 min of a block or of the previous
  wait, until the gate allows a stop; one short wait and then real work is healthy. A wait
  killed before it returned (a credential switch, a crash) has a PreToolUse but no result of
  the same `tool_use_id`; it is closed at the session bead's `ended_at` (else `closed_at`),
  or at the sweep's now while the session runs ("still waiting"), and skipped when no
  session bead covers it (gd-c6q4LOONzY).

## Regression fixtures

Every class in the survey ([session-sweep-survey.md](session-sweep-survey.md)) has real,
redacted session excerpts under `gardenio/controller/cmd/gb/testdata/session_sweep/<class>/`:
at least one positive (or a known miss) and one negative, a healthy session that looks
similar. `TestSweepDetectsSurveyedClasses` runs the sweep with its default thresholds over
all of them (no network, no daemon), so a detector change that loses a class, or flags a
pinned false positive, fails `go test ./cmd/gb/`. The fixture README says how to capture one
from a session id (`gb audit fetch`, `kd show --json`, `transcript_to_bus.py`).

Caught, by pattern: `ci-poll`, `cold-compile`, `idle-wait` and `local-golden-suite`
(`slow-call`); `restart-loop` (with the three survey false positives pinned as negatives);
`repeat-call`; `dup-comment`; `missing-tool` (`harness-gap`); `own-bead-403` (`auth-storm`
inside the bus window, `stop-did-not-take` off it); `gh-token-scope`; `ownership-403`; `infra-access-gap`;
`browser-deps-eacces`; `pkill-self-match`; `tmpdir-unset`; `wrapup-quoting`;
`stop-refused-claimed-schedule` (`stop-refused-schedule`); `ci-retrigger-churn`
(`retrigger-churn`); `bead-history-reread` (`bead-reread`); `stop-did-not-take`;
`killed-mid-task`; `interrupt-then-crash`;
`noop-followthrough`; `dispatch-collision` (on a PR); `oom`; `stop-gate-wait` (a block,
then 2+ min of foreground waits, from the gate's Stop events).

Known misses, each a skipped fixture naming its bug: none open.
