# Session sweep survey: problem classes in archived agent sessions

This survey reads archived agent sessions to find the problem classes that `gb session sweep`
([session-sweep.md](session-sweep.md)) needs to catch. The goal is to build its detectors from
problems that actually happen, not only from the founding case (crew-gardenio-infra-5juu). It was
done on 2026-10-30 for plan gd-xV14ol0GhR, step gd-rFKHFx1Gs1. The fixture step gd-JCoNrApHwa
turns each class below into a regression test, and gd-BvrgCCOXWC works the fixes.

In short: the sweep's seven detectors cover a minority of what goes wrong.

- **Waiting.** The biggest costs are waiting: cold Go compiles (61 sessions with a wait of
  10 minutes or more), CI polled from inside a single tool call (43 sessions), and local golden
  suites (10 sessions).
- **Half of these are invisible to `slow-call`.** That detector reads `slowest_calls`, which is
  written only for sessions that end `completed` or `crashed`. 87% of sessions end `retired` and
  carry no metrics at all.
- **Two common classes have no detector.** The GitHub token cannot read check runs or re-run
  jobs (35 agents). A stop that never lands leaves a pod idle for hours (5 sessions, about
  13 pod-hours).
- **Several cheap, mechanical classes are missed too:** `pkill -f` killing its own shell,
  `$TMPDIR` unset, inline `--wrapup` JSON breaking shell quoting, and scheduled runs whose stop
  is refused.

## Sample

| | |
|---|---|
| Archive | 11,910 session beads, from 2026-09-18 (not 2026-10-27; richer fields start 2026-09-23 to 2026-10-11) |
| Window | 2026-10-27T00:00Z to 2026-10-30T06:36Z: 2,682 sessions (673 / 845 / 831 / 333 a day) |
| Roles in window | plan-gardener 1,016, crew 971, reviewer (merge desk) 498, thread 99, infra 44, cicd 40, short-title-groomer 6, crew-fable 4, release-captain 3, plan-captain 1. No session bead in the window has role `gardener`, `merge-desk` or `captain`; the merge desk runs as `reviewer`. |
| Looks-wrong flags | 431 sessions (16%) had at least one flag: settled cost in the top decile (≥ $0.67, 228), more than 3 sessions a day for one agent (130), `restart_cause` credential_switch (87), request_count > 40 (46), crash (38), exit 130 or -1 (4). No session in the window has `compaction_count > 0` or `error_count > 0`. |
| Sample | **251 sessions**: every crash or odd exit (41), every request_count > 40 (43), 50 top-cost, 30 more-than-3-a-day, 15 credential_switch, and 72 drawn at random from each role on each day (3 per role-day for crew, plan-gardener and reviewer, 2 for the rest). By day: 38 / 72 / 89 / 52. By role: crew 110, infra 37, reviewer 33, plan-gardener 31, thread 19, cicd 8, short-title-groomer 5, crew-fable 4, release-captain 3, plan-captain 1. |
| Transcripts | 208 of the 251 matched an S3 archive (`gb audit fetch <agent-bead>`, matched on start time), and **195** had a non-empty transcript. All of them were read through a digest (one line per call, result, agent text and hook block). The 43 with nothing archived are mostly crash-loop sessions under 30 s long (5juu, bi5m, vdzc). |
| Extended search | A pattern search over all **281** archived transcripts of the 210 sampled agents. Classes marked *grep* in the table below were counted this way. |
| Wrap-ups | 2,464 agent beads created in the window: 2,399 with a wrap-up, 452 with `harness_gaps`, 2 with `blockers`. |
| Clean | 54 of the 195 read sessions (28%) had no problem worth a detector. |

A resumed session's archived transcript also contains the turns it resumed from (53 of the 251
sampled sessions are `resumed`). The tables count both **sessions** and **distinct agents**, and
the agent count is the safer number.

## Classes

The status column says how the current sweep handles each class. **caught**: a detector exists and
found the cited cases. **partial**: a detector exists but misses a measured share. **missed**: no
detector. **needs threshold**: a detector exists, but its threshold or grouping is wrong for this
class.

Cost is the wall time the problem added, estimated session by session by the reader, and the
token spend for that time: the session's `settled_cost_usd` × (problem minutes ÷ session
minutes). An idle pod costs node time, not tokens, so a large minute figure can go with a near-zero
dollar figure.

| # | Class | Signal | Sessions (agents) in sample | Cost | Sweep |
|---|---|---|---|---|---|
| 1 | `ci-poll` | one Bash call ≥ 10 min whose input polls CI (`gh run watch\|list`, `gh api …/actions/runs`, `until … gh`) | 46 (35); 43 ≥ 10 min | ~2,250 min, ~$30 | **partial**: `slow-call` ci-wait found 13 of 43 |
| 2 | `cold-compile` | `go test`/`go build`/`go vet`/`golangci-lint` call ≥ 10 min, with `go: downloading` in the output or a timeout | 64 (39); 61 ≥ 10 min | ~2,150 min, ~$24 | **partial**: `slow-call` build-test found 32 of 61 |
| 3 | `gh-token-scope` | tool output `Resource not accessible by personal access token` (statusCheckRollup, check-runs, `gh run rerun`) | 36 (35); 204 of 452 wrap-up `harness_gaps` | ~190 min direct, plus a full CI cycle per empty-commit retrigger | **missed** (by design: `harness-gap` ignores permissions). Fixing it needs wider access, see below. |
| 4 | `own-bead-403` | `HTTP 403: token not authorized for this bead's project` on the agent's own agent bead, during `gb stop` or the Stop hook | 6 (6) | ~1,110 pod-min (idle), ~$0.21 | **partial**: `auth-storm` matches it, but only inside the ~3–5 h bus replay; 0 found over the window |
| 5 | `stop-did-not-take` | `gb stop` returns an error, hangs, or is never reached; the session ends 30 min or more after its last transcript event (not `retired`), or a blocking `gb decision create` holds the pod | 5 (5) | ~770 pod-min | **missed** |
| 6 | `restart-loop` | an agent with more than 3 sessions in one hour, each re-running a finished task | 3 sessions read (fleet: 3 agents, 34 sessions, ~1,900 pod-min) | ~1,900 pod-min, $3.48 | **caught**, but 3 of 7 findings were false positives (see below) |
| 7 | `noop-followthrough` | a session spawned for an escalation or decision whose task is already closed; the agent verifies, says "nothing left to do", and stops | 7 (6) | ~8 min, 7 pods | **missed** |
| 8 | `ownership-403` | `HTTP 403: ownership denied: update on bead … requires created_by, assignee, owner, or admin role` on a bead the agent was sent to work (inherited, pre-assigned, or a plan step) | 9 (4); 48 wrap-ups | small per call; each case becomes an admin escalation | **partial**: `auth-storm` counts only the agent's own bead and `task_id` |
| 9 | `killed-mid-task` | session ends `restart_cause=credential_switch` (exit 10) or `crash` mid-call, with no `gb stop`; often right after `API Error: Connection refused (ECONNREFUSED)` | 19 (18) | ~100 min of lost calls; uncommitted work lost in 2 | **missed** |
| 10 | `interrupt-then-crash` | `[Request interrupted by user]` or `The user doesn't want to proceed with this tool use` in the first minutes, then exit 143 | 8 (7) | ~16 min, $2.13; one merge landed while shown as rejected | **missed** |
| 11 | `stop-gate-wait` | `[gb hook stop-gate]: {"decision":"block","reason":"decision: decision point offered before session end"}` while the agent waits on a background job, Monitor or subagent | 9 (9) | small directly; it causes the foreground waits in `idle-wait` and `ci-poll` | **missed** |
| 12 | `idle-wait` | a foreground `until … sleep`, `pgrep` or `tail -f` loop ≥ 5 min waiting on the agent's own background job | 12 (12) | ~300 min, $3.32 | **partial**: `slow-call` wait-on-process, at ≥ 10 min only |
| 13 | `local-golden-suite` | a full `TestGardenGoldens` or browser suite run in the pod (13–39 min), often hanging on WebGL splash pages or hitting go test's 10 min default timeout | 10 (10) | ~530 min | **caught** (`slow-call` goldens 9 of 10), but `make commons-ux-shoot` is misfiled as build-test |
| 14 | `ci-retrigger-churn` | 3 or more `git merge origin/main` or `--allow-empty` pushes in one session to re-run CI or chase main | 7 (7) | ~455 min, $3.22 | **missed** |
| 15 | `browser-deps-eacces` | `make gardenhub-browser-deps` runs ~8 min, then `EACCES: permission denied, mkdir '/ms-playwright/__dirlock'` | 6 (6) | ~47 min | **missed** (under the 10 min slow bar) |
| 16 | `missing-tool` | `command not found`, `unknown command "pr" for "gb"` | 11 (11) | ~3 min | **caught** when the wrap-up names it (`harness-gap`) |
| 17 | `pkill-self-match` | `pkill -f <pattern>` returns `Exit code 144` (it matched the agent's own shell) | grep: 14 (8) | ~1 min each; the call's other work is lost | **missed** |
| 18 | `bead-history-reread` | the same bead `kd show`n 4 or more times in one session, or a `kd show` over 30 KB (`Output too large (5x.xKB)`) | grep: 27 (21) | context tokens on every wake | **missed** (`repeat-call` needs 5 identical inputs; the pipes vary) |
| 19 | `tmpdir-unset` | `/<name>: Permission denied` from a command that wrote `$TMPDIR/<name>` | 4 (4) | seconds | **missed** |
| 20 | `wrapup-quoting` | `gb stop --wrapup '…'` fails with `eval: line 1: syntax error near unexpected token` or `unexpected EOF while looking for matching` | grep: 5 (4) | seconds; a retry | **missed** |
| 21 | `stop-refused-claimed-schedule` | `gb stop` refused with `Warning: you have claimed in-progress work: … Schedule: …` in a scheduled run | grep: 6 (6) | seconds; a retry | **missed** |
| 22 | `dispatch-collision` | two agents on one bead or PR, or crews sent to beads a merged PR already fixed | 4 (4) | ~16 min | **missed** |
| 23 | `infra-access-gap` | `system:serviceaccount:gardenio:gardenio-infra-reader" cannot list resource "rolebindings\|ingresses\|networkpolicies"`, or the public edge refuses pods | grep: 5 (4) | checks left unverified; 5juu looped on it | **missed**. Fixing it needs wider access, see below. |
| 24 | `oom` | the pod restarts mid-`golangci-lint`, mid-`go vet` or beside Chromium; the next session says "probably out of memory" | 1 read; 4 wrap-ups name it | a lost call and module cache | **missed**: no window session has exit 137, see below |
| 25 | `repeat-call` | the same tool input 5 or more times in one session | 2 read + 5juu (18×) | inside `restart-loop` | **caught** within the bus window |
| 26 | `dup-comment` | the same comment text twice or more on one task bead | 6 sweep findings, all real | noise | **caught** |

Known classes that were not observed, and what was checked:

- **Interactive-tool stalls** (AskUserQuestion, EnterPlanMode, ExitPlanMode, EnterWorktree):
  0 calls in 281 transcripts.
- **Compaction thrash:** 0 compaction markers in 281 transcripts, and `compaction_count` is 0 on
  every window session that has the field. 152 sessions compacted before 2026-10-17, none since.
- **Decisions filed but never acted on:** 0. Every `gb decision create` was followed by
  `gb yield` or `gb stop`. The one problem was `gb decision create` blocking by default
  (`stop-did-not-take`).
- **Escalations for access the agent already had:** 0 found.
- **Thread agents that answer and then linger:** 0. Thread sessions that end `completed` end
  within 0.3 min of their last event. The apparent linger is `retire-lag`, below.
- **Prime/news context before any work:** not a problem. The first work-shaped call (edit, commit,
  claim-and-comment, squawk) comes at request 4 (median), 14 (p90). The 19 sessions that spent
  over half their input tokens before it were short no-op runs (`noop-followthrough`).
- **Claiming a bead another agent holds:** 0 cases of `kd claim` on another agent's bead. The
  2 conflicts were an agent already holding its own first bead, in two-bead assignments.
- **Wrap-ups with an ask and no handle:** 1 ask-gate block (gd-msYFntL7QR), below the
  three-session bar.

Seen once, below the bar for a class: a `sleep 600 &` child holding a pipe open for 10 min per
call (~75 min, gd-51GO5t8vyD); a `timeout` that killed `go build` and was read as success
(gd-4VpvmGnBHt); `kd create` refused for a `short_title` over 40 characters, with the blank id
then passed to `kd dep add` (gd-3ZV8avi7ur); an async research subagent launched and never read
(gd-k6nwtjUSlU).

## Examples

Each class lists three or more sessions (session bead ids, `kd show <id>`), with the excerpt that
shows it. The full per-session hit list, with timestamps, is in the comments on gd-JCoNrApHwa.

1. **`ci-poll`**
   - gd-BqrCf47mw4 (crew): `RESULT (2678s) 06:35:19 ⏎ 40825714715 checks completed success`. A 45-min foreground `gh api` poll, after an earlier 40-min one.
   - gd-J9BgbJwiZ4 (crew, agent g0p): `Bash 1h20m1s: q() { gh run list --branch agent/one-eventsource/…`.
   - gd-Jbk3V2scy4 (reviewer): `RESULT (940s) deadline reached, 2 runs pending`.
   - gd-gQdbYBTnqh (crew): `RESULT (844s) ci success … images success … checks success`.
2. **`cold-compile`**
   - gd-AVdc4Bbt43 (crew): `RESULT (1014s) go: downloading github.com/go-openapi/swag/yamlutils …`. A targeted `go test ./internal/bridge` took 17 min.
   - gd-784c3T3tZ5 (crew): `RESULT (1501s) go: downloading …`. It hit its 25-min timeout, then the rerun took as long again.
   - gd-J4VfDV9Bjv (crew): `RESULT (1204s) Command did not complete within its 1200s timeout and was moved to the background`.
   - gd-BqrCf47mw4 (crew): one `TestCardUnderChromium` run took 15 min, mostly module downloads.
3. **`gh-token-scope`**
   - gd-51GO5t8vyD (crew): `GraphQL: Resource not accessible by personal access token (node.statusCheckRollup…)`. Its CI wait loop could never see completion.
   - gd-Jbk3V2scy4 (reviewer): `run 40690866579 cannot be rerun; Resource not accessible by personal access token`.
   - gd-gQdbYBTnqh (crew): the same rerun refusal on two runs; it toggled draft/ready to retrigger.
   - gd-BqrCf47mw4 (crew): could not rerun a known flake, so it pushed an empty commit and waited a full CI cycle.
4. **`own-bead-403`**
   - gd-3VUxPYr0A0 (infra, vfyw): `Warning: could not store wrap-up: reading bead gd-FNspwdzw43 for field update: HTTP 403: token not authorized for this bead's project`. The pod then sat idle for about 7 h.
   - gd-4k90UoObSA (infra, 5juu): `getting bead gd-f6cpVDez1j: HTTP 403: token not authorized` on `gb stop`; idle about 4 h.
   - gd-9n0HMYKFqc (infra, bi5m): `This is the ninth attempt, and it returns the same 403 on my agent bead gd-QwSwZRrDZ8`.
   - gd-oU68flndzb (plan-gardener): `Error: getting bead gd-lcEbOQSeZz: HTTP 403 …` on its own agent bead; idle about 80 min.
5. **`stop-did-not-take`**
   - gd-s8woX8LSuM (thread): `RESULT ERR (201s) [Tool call interrupted: the session ended before this call's result was recorded…]` on `gb stop`. The pod respawned with the same prompt.
   - gd-NGjwr02Whe (plan-gardener): `The create call was blocking until someone answered`, from `gb decision create` without `--no-wait`; 1,800 s held.
   - gd-3VUxPYr0A0, gd-4k90UoObSA, gd-oU68flndzb: the session ended 432, 228 and 74 min after its last transcript event.
6. **`restart-loop`**
   - gd-9n0HMYKFqc (bi5m): `This is the same task again, and the check and the comment (33020) are already done. I'll retry gb stop.`
   - gd-h5cUgq6AEY (vdzc): the same read-only check was delivered 5 times (02:05, 04:42, 04:53, 05:03, 05:14) because the stop never landed.
   - gd-y1ME6TAgTM (vdzc): respawned for its own despawn escalation, with nothing to do.
   - The 5juu sessions have no archived transcript: 19 sessions in the window, 15 of them `crash`.
7. **`noop-followthrough`**
   - gd-DFpIwnuoTL (crew): `"tldr":"Follow-through for gd-wj4tW0LDas: PR #1309 already merged as 391ab47f and gd-XWocW0x1fq already closed; nothing left to do"`.
   - gd-4Ho160WZpc (thread): `PR #1498 … is merged … and the task is closed; nothing …`.
   - gd-H1QP4BDUiz, gd-cWJ1jx5w4W, gd-hFXyb78oRq, gd-2hlavvRv4m: the same shape.
   - gd-HqDb16HPUn (crew) was spawned on a merged task and chose to watch main CI instead of stopping.
8. **`ownership-403`**
   - gd-6isS0wbKW7 (crew): `Error: claiming bead gd-gayBaTEoRL: HTTP 403: ownership denied` on its own pre-assigned task.
   - gd-Altly08bQf (crew): `All 8 closes returned 403 (gd-ThtCuqFkzl), so I'll file an admin escalation.`
   - gd-Y05INotScd (thread): `HTTP 403: ownership denied: update on bead gd-lS2J2wOJTR requires created_by, assignee, owner, or admin role`.
   - gd-5B5YSLt4GB (crew): `escalation created but could not link blocks-edge from gd-POzToXXv3L … HTTP 403: ownership denied`.
9. **`killed-mid-task`**
   - gd-2TZxTaEwbP (crew): `credential_switch` at 04:21Z on 10-29 killed `go test`, with about 20 test-file patches uncommitted.
   - gd-J9BgbJwiZ4 (crew): `API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)`, then `credential_switch` at 16:07Z.
   - gd-6IQAgC784L (crew): killed during `go test -run 'TestWindow_BearerOutsideW…'`, with no `gb stop`.
   - gd-EekuBlS55t (crew): killed during `golangci-lint`; the work was pushed but no PR was opened.

   Two fleet-wide `credential_switch` events account for most of these: about 04:21Z and
   16:03–16:07Z on 2026-10-29.
10. **`interrupt-then-crash`**
    - gd-nt8aZJyoKQ (reviewer): `You rejected the last call, but its side effects landed anyway: PR #1283 is merged.`
    - gd-b8WjhEvfXw and gd-HqDb16HPUn: `[Request interrupted by user]` in the same minute (20:10Z, 10-28), then both exited 143.
    - gd-g9divCIDRt (reviewer): interrupted 40 s in, then exit 143.
    - gd-Dpvzq7PMKc, gd-9PkWICwsdl: `The user doesn't want to proceed with this tool use`, then exit 143.
11. **`stop-gate-wait`**
    - gd-J2p7YvgBGk (reviewer): `Stop hook feedback: [gb hook stop-gate]: {"decision":"block","reason":"decision: decision point offered before session end"}`, twice while waiting on subagents.
    - gd-EQGxYgKBVv (crew-fable): `The gate won't let the turn end while I wait, so I'll block on the golden run in the foreground`.
    - gd-Ybnt19FYfq (crew): `The stop gate won't let me end the turn while CI runs, so I'll poll CI in the foreground instead.`
    - gd-RgV3ZR3utI (crew): `The poll loop exited at once: foreground sleep is blocked here. Switching to the Monitor tool.`
12. **`idle-wait`**
    - gd-Tq7OTxuFMy (crew): `until ! pgrep -f "go test -c" …; do sleep 5; done` ran into `Command did not complete within its 1800s timeout`.
    - gd-x5mSUxCae6 (thread): two foreground `until grep -q '^exit=' …; do sleep 5; done` loops (1,502 s and 1,302 s).
    - gd-FBwSfklkHH (crew): a 22-min sleep loop on a background golden run that then hit `panic: test timed out after 35m0s`.
13. **`local-golden-suite`**
    - gd-J4VfDV9Bjv (crew): `--- FAIL: TestGardenGoldens (1924.91s)`; runs of 28 and 32 min died on a splash timeout.
    - gd-p2Z4Ni8IpA (crew): 9 local golden or browser sweeps of 13–34 min each (about 3.3 h).
    - gd-7MCuhQyXZV (crew): `FAIL gardenio/controller/internal/gardenhub 600.189` (go test's default timeout), twice.
    - gd-RgV3ZR3utI (crew): `The stash was empty because the change was already committed, so that shoot is really "after".` A 27-min UX shoot was wasted.
14. **`ci-retrigger-churn`**
    - gd-LWbqsCQHF6 (crew): `Other open PRs … are also red on the perf gate, which points to gate noise`. A 1-line CSS PR took 3 retriggers (about 3 h).
    - gd-EQGxYgKBVv (crew-fable): `CONFLICT (content): Merge conflict in …/golden/commonslightbox-desktop-dark.png`. Its goldens were regenerated, then conflicted again within 30 min.
    - gd-L2GWqXFMLg (crew): `main's own ci (e7e45883) fails TestWorldFrameBudgetUnderNode the same way`.
    - gd-RgV3ZR3utI (crew): `The token can't re-run jobs, so I'll push an empty commit to re-trigger CI.`
15. **`browser-deps-eacces`**
    - gd-8yi8C6JvFx: `Error: EACCES: permission denied, mkdir '/ms-playwright/__dirlock'`.
    - gd-7Jqyz1hjmG: `RESULT (491s) … Failed to install browsers`.
    - gd-LWbqsCQHF6: `RESULT (494s) …`.
    - gd-p2Z4Ni8IpA: `RESULT (492s) chromium-1208 …`.
16. **`missing-tool`**
    - gd-gQdbYBTnqh and gd-RgV3ZR3utI: `Error: unknown command "pr" for "gb"`. The prompt named `gb pr`, which exists on main but not in the deployed gb.
    - gd-UblzDp9fwh: `column: command not found`.
    - gd-ohWK6NtJ9N: `gofumpt: command not found`.
17. **`pkill-self-match`**
    - gd-Tq7OTxuFMy: `My pkill pattern matched its own shell.`
    - gd-NGjwr02Whe: `pkill -f "gb decision create --requesting-task gd-G5PmhWXsF9"` gave `Exit code 144`.
    - gd-p2Z4Ni8IpA: `pkill -f "TestLandsPerfBaseline" …` gave `Exit code 144`.
    - Also gd-EQGxYgKBVv and gd-51GO5t8vyD.
18. **`bead-history-reread`**
    - gd-9n0HMYKFqc: `kd show gd-udek4CJVRY` 14 times.
    - gd-r6ZGwJuRgN (merge desk fd2r): `kd show gd-XiCHQfLiKI` 7 times.
    - gd-CmJD2okPFo (plan-gardener x2gi): the same bead 6 times.
    - gd-Altly08bQf (crew): `Output too large (66.5KB)` from `kd show` of a task bead with about 400 comment lines, re-read on every wake.
19. **`tmpdir-unset`**
    - gd-b8WjhEvfXw: `/bin/bash: line 1: /esc.json: Permission denied`.
    - gd-AudP7JFycb: `/b.md: Permission denied`.
    - gd-A3Ao7c4jnh: `/trig.json: Permission denied`.
    - gd-UDGDWrwM7F: `/esc.json: Permission denied`.
20. **`wrapup-quoting`**
    - gd-OsenjtisLd and gd-vPnxY3CBsi (plan-gardener): `` eval: line 1: syntax error near unexpected token `(' … gb stop --wrapup '{"tldr":"Plan gd-xV14ol… ``.
    - gd-8Vbqfd9HZp (crew): `` unexpected EOF while looking for matching `"' ``.
    - gd-jfutjb06p6 (plan-gardener): the same error.
21. **`stop-refused-claimed-schedule`**
    - gd-yg5M5DDzU3, gd-SsfUOLtMOm, gd-mauBCzzVtr (cicd watchdog): `Warning: you have claimed in-progress work: gd-JPwD5bvgkM: … Schedule: cicd watchdog…`.
    - gd-TTICGlJzVk, gd-SktF5JuxcW, gd-ipUCPIg5oj (short-title groomer): the same, on their schedule beads.
22. **`dispatch-collision`**
    - gd-R5EpPupOyS (thread): `This is a duplicate tend. Another thread agent landed the work on PR #1246 fifteen minutes ago`.
    - gd-qpZbLKmEKZ (reviewer): merged main into #1204 while the escalation executor was merging it.
    - gd-vu9TtDtzot (plan-gardener): `merged PR #1345 already fixed several beads that last run's batch crews were sent to fix`.
    - gd-PTLM9eHnd5 (crew): `weOKYDBRGQ/yixSAIvEEf already on main via #1345`.
23. **`infra-access-gap`**
    - gd-0gZiQx0eUo (z5zn): `gardenio-infra-reader" cannot list resource "rolebindings"`.
    - gd-3vFU85ZGjt (1cs6): `cannot list resource "ingresses"`, plus `Failed to connect to gardenio.clonk.tv port 443` from the pod.
    - gd-4k90UoObSA (5juu): `cannot list resource "ingresses"`; the check it was re-running needed this.
    - gd-h5cUgq6AEY (vdzc): `cannot get resource "keycloakclients"`.
24. **`oom`**
    - gd-IZ0S2904pT (crew): `The last session ended during the lint run, probably out of memory.`
    - Wrap-ups: gd-5Hyt1023H5 (`golangci-lint over internal/bridge exceeds the 2Gi pod limit and restarts the session`), gd-hBVuM7Fn6A (`golangci-lint on one package exceeded the 2Gi pod limit`), gd-mvJ7O9mVol (`go vet and golangci-lint on cmd/gb together restarted the pod mid-call, and the Go module cache was wiped`).
    - Their session beads end `retired` or `credential_switch`, never with exit 137.
25. **`repeat-call`**
    - 5juu (agent bead gd-f6cpVDez1j, coop session 6fdde730): `kubectl -n gardenio get pods … | grep -i 'viewer\|edge\|proxy\|ingress'` 18 times.
    - gd-9n0HMYKFqc (bi5m): the same `kubectl … get pod $P -o jsonpath=…` 8 times, and the same `gb stop --wrapup` 9 times across respawns.
    - gd-h5cUgq6AEY (vdzc): the same `kubectl -n operators get keycloakclient …` on each of 5 respawns.
26. **`dup-comment`**
    - gd-EvK4BmuKos and gd-yLI1Ze1XYD: `Fixed in PR #1282, merged to main as 72b161b6` posted twice, 4 s apart, by agent y6i.
    - gd-3F4W22h2nT: `machine:gardenio-trigger-engine` posted `gardener gd-UbSu3KNA89 parked behind gd-0BA0z9yt7T and will not spawn until it clears` 3 times in 16 h.
    - gd-WFMvg9sDKE: the viewer's `The owner's approve … is not applied yet` 3 times.

## How the existing detectors did

The sweep was built from main (768c9127) and run over the same window:
`gb session sweep --since 82h --bus-timeout 120s --json` at 2026-10-30T07:17Z. It read 2,765
sessions, 3,281 agent beads and 1,243 task beads in 19 s. The bus replay returned only 671 events,
covering 03:52Z–07:16Z (3.4 h). Each finding was checked by hand against the transcript, or
against the bead for comments.

| Detector | Findings | True | False positive | Recall against the sample | Notes |
|---|---|---|---|---|---|
| `restart-loop` | 7 | 3 (5juu, bi5m, vdzc); 1 partial (thread-1793147913-034359: a hung `gb stop` respawn among legitimate thread resumes) | **3 (43%)** | 3 of 3 true loops | The false positives are wake-driven: plan-gardener epfq (3 short scheduled runs and a decision), 911c-nursery-backfill-6wz (21 decision-resume wakes, one per card), close-the-orphans-zm8 (one continuous run split by a sigterm respawn). Suggestion: count only sessions that re-run a task already closed, or skip a session whose agent bead has a `wake_reason` (decision, escalation or thread resume). |
| `auth-storm` | 0 | – | – | 0 of 15 | The bus window held none of the sample's 403 sessions. The detector cannot see past the replay. `own-bead-403` would be visible without the bus: an agent bead with no stored wrap-up, `stop_requested` unset, and a session idle for more than 30 min. |
| `oom` | 0 | – | – | 0 of 4 | No session in the window has `exit_code` 137 (exit codes seen: 143 ×228, 10 ×87, 130 ×3, -1 ×1, absent ×2,363). The OOM kill takes the container, and the session is later stamped `retired` with no exit code. |
| `repeat-call` | 0 | – | – | 0 of 3 | Bus window only. All three cases were older than the replay. |
| `dup-comment` | 6 | **6** | 0 | – | Two are an agent (y6i, two beads, 4 s apart). Four are system actors (trigger-engine ×3, beads-viewer ×1). The transcript repeats found by search (5juu 18×, bi5m 8×, vdzc 4×) were mostly retries that got 403 and never posted, so missing them is correct. |
| `slow-call` | 153 (67 sessions, 60.7 h) | 153 (every call is truly ≥ 10 min) | 0 on duration | ci-poll 13 of 43, cold-compile 32 of 61, local-golden-suite 9 of 10 | **Kind is wrong on 15 of 153 (10%)**: 13 `make commons-ux-shoot` runs filed as build-test (`make` matches before any golden word), 1 `tail --pid` wait filed as other, 1 chained command. Recall is low because only 311 of 2,682 window sessions have `slowest_calls` (see below), and because many polls are split into calls under 10 min. |
| `harness-gap` | 18 | 16 | **1 (6%)**, plus 1 borderline | – | False positive: "No way to re-run a flaky CI job with the pod GitHub token (403 …)", which is a permission. Borderline: "There is no GPU in the pod". The subject grouping fails on 6 of 18: 4 gofumpt/staticcheck sentences grouped as `(none)` and 2 as `way`. 7 of the 18 are `gb pr` or `gb session` version skew: the verb exists on main but not in the deployed gb, so it is a release gap, not a missing tool. |

## Telemetry gaps that limit the sweep

- **Retired sessions carry no metrics.** 87% of window sessions (2,324 of 2,682) end `retired`.
  They have no `exit_code`, `request_count`, `error_count`, `compaction_count` or `slowest_calls`.
  From 2026-10-12 to 2026-10-16, 97–100% of sessions had these fields; since 2026-10-17
  about 11% do. This alone
  halves `slow-call` recall and blinds `oom`. The fix is for coop, or the retire sweeper, to
  write the end-of-session metrics on retire.
- **`ended_at` on a retired session is the retire time, not the end** (`retire-lag`). In all
  115 retired sessions read, `ended_at` falls about 16 min (median) after the last transcript event.
  Examples: gd-tfGqNRxMqp, gd-Y5pB5BR8Ze, gd-4P5Q3DmSoi, gd-hf31Wlpuhp. Any duration or
  linger measure built on `ended_at` must allow for this. Whether the pod itself lives those
  16 minutes needs a read-only cluster check (an infra spawn); this survey did not do one.
- **The bus replay holds hours, not days.** `auth-storm` and `repeat-call` can only ever see the
  last ~3–5 h. A sweep run once per gardener cycle will see each storm only if a cycle falls inside
  it.
- **The S3 archive is keyed by agent bead, not session bead.** `gb audit fetch <agent-bead>`
  returns every coop session under it in about 1 s. `gb audit list --since 6h` took 2 min 7 s.
  Matching a session bead to its transcript means comparing start times; the coop uuid is not
  on the session bead (`session_log` is the Claude transcript path, a different uuid).

## Fixes that need wider access

These classes cannot be fixed by an agent within its current access. Per gd-BvrgCCOXWC, they go to
the operator as a decision with the least-privilege change. They are listed here and noted on that
bead; nothing was widened.

- **`gh-token-scope`.** The fleet GitHub token cannot read `statusCheckRollup` or check-runs and
  cannot re-run jobs. The narrowest fix is read access to commit statuses and check runs, plus
  Actions write (re-run) on this one repository. If a fine-grained PAT cannot grant check-run
  read, a GitHub App installation token is the narrow route. The alternative is to leave it, and keep paying a full CI cycle (empty commit) per flake plus hand-rolled pollers that
  misread a 403 as pending (gd-51GO5t8vyD).
- **`infra-access-gap`.** The `gardenio-infra-reader` ServiceAccount cannot list `rolebindings`,
  `ingresses` or `networkpolicies`, or get `keycloakclients`, so infra checks that need them stay
  unverified. 5juu's loop re-ran exactly such a check. The narrowest fix is read-only `list`/`get`
  on those four resources, in the namespaces the checks use (`gardenio`, and `operators` for
  `keycloakclients`). Access to Secrets should stay denied (gd-7xcuamOIC9
  was refused `secrets`, which is correct). The public edge refusing pod traffic
  (`gardenio.clonk.tv:443`) is a network-policy choice, for the operator to keep or change.
- **`own-bead-403`** (a token not authorized for its own bead's project) and **`ownership-403`**
  are already in progress as gd-WKjtvzPl6g and gd-G5PmhWXsF9. They are token and ownership rules,
  not new scopes.

## Reproducing

The working files (sample index, digests, per-batch hits) were in the survey agent's scratchpad
and are gone with its pod. To redo it:

```sh
kd list --type=session --json --limit 500 --offset N --sort created_at   # page all session beads; 500/page, sequential (parallel pages got HTTP 500)
kd list --type=agent --all-types --json --created-after 2026-10-27T00:00:00Z --limit 500 --offset N
gb audit fetch <agent-bead-id> --output DIR   # every archived session for one agent, ~1 s
gb session sweep --since 82h --json           # from a gb built at or after 768c9127
```

Match each session bead to a transcript by `agent_bead_id` and `started_at` (meta.json
`started_at`, within 10 min). Read the `type: assistant` `tool_use` and the `type: user`
`tool_result` entries. A call's wall time is the result's timestamp minus the call's timestamp.
