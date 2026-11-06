# Agent access programme: agents build software without pointless human steps

Owner ask, 2026-11-02: "look at all open escalations and decisions (and recent
closed ones) and develop a programme for fixing agent access so they can develop
software for us without pointless human intervention", then "consolidating and
continuing the existing plans is fine but we should make sure from here that
they get done".

This document is the programme's index. It adds no new plan epic. Every step is
a task in one of three plans that are already `building` and already have a
bound gardener:

| Plan | Epic | Gardener | New phase(s) here |
|---|---|---|---|
| Escalation routing | gd-KAX4iUAI42 | gd-Sx6JqYiFuI | P7 Execution never stalls, P8 Agents act and tell, P9 Standing access |
| Ownership grants | gd-fRmqrpHIwk | gd-zMkyNpX8ks | P-G Dispatched-task grant, gardener self-idle |
| Releases | gd-UZKZ9H25UD | (its bound gardener) | P-R Releases ship themselves |

The steps already in flight in those plans stay as they are and count toward
this programme: escalation-routing Task 14 (gd-VJ7Pti7c5q, laptop switch-over and
the `/result` gate) and Task 15 (gd-sZNUPjzygT, gd-yT5xAPB0uJ, gd-8gfzBvgFrO, the
in-cluster `coordinator:cluster` executor lane).

## Evidence (2026-10-19 .. 11-02)

Two read-only analyses of every open escalation and decision plus the newest
2000 closed escalations and 405 closed decisions.

**Escalations.** 2097 in the window. 1539 were PR merges; the merge desk's
coordinator approval took those from 58 owner clicks a day (10-27) to 2
(11-02), so merges are solved. The other 558 drew **302 owner clicks in 12
days**, and the laptop Claude session executed 61% of them. Grouped by the
access gap that forced them:

| Cluster | Total (open) | Owner judgment needed? |
|---|---|---|
| A. "Roll PR #N in the next window" + edge applies | 136 (26) | Only for cluster-scoped values, ordered security rolls, failed rolls |
| B. kbeads ownership 403 on the bead the task names | 85 (6) | No |
| C. admin-only bead writes (tunables, release records, plan lifecycle, heraldry) | 47 (10) | Only advice (fleet prompt content) |
| D. trigger edits/reseals (budget raises after an "extend" ruling, desk prompt installs) | 43 (5) | Only the desk prompt's content |
| E. privileged spawns | 66 (0) | Mostly sealed already |
| G. GitHub token scopes and Actions re-runs | 21 (15) | The App/PAT mint only |
| H. terraform and AWS | 23 (0) | Applies, yes; reads, no |
| I. agents cannot observe prod (curls, viewer pages, kubectl reads) | 22 (6) | No |
| J. chart and agent-image edits kept out of the crew lane | 12 (7) | Only RBAC, cluster-scoped kinds, Secrets |
| K. secrets and credentials | 12 (5) | Yes (floor) |
| L. owner device, browser, external accounts | 13 (3) | Yes |
| M. authz design changes filed as escalations | 12 (7) | Yes, as decision + PR |

Process failures on top of the gaps:

- **64 approved, never executed** (median 35 h since approval). The 6 h idle
  hand-up moves `held_by` to the owner but `execute_by` is frozen
  (`escalationFrozenFields`, `kbeads/internal/server/escalation_audience_authz.go:49`),
  so the owner's click dispatches to nobody. Agents pinged them 244 times.
- 41 escalations cite an owner decision already taken and were approved a
  second time.
- 16 bounces: an executor pod was spawned for work it had no identity for.
- Duplicates: three checks:read asks (gd-Lj5RQ4qjkc, gd-GcmGV669ex,
  gd-nMPLxDgPhq), three Actions re-run asks, three viewer-tunables asks, five
  ownership-gap asks.
- 126 free-text `operation_type` values, so policy rules keyed on type miss and
  items fall to the operator-only catch-all. The policy, not the code floor,
  sends almost everything to the owner: the floor forced 6 of 558.

**Decisions.** 413 in the window, about 27 a day. The owner picked the agent's
first option 77% of the time (79% when one was marked recommended). 36% were
genuine owner judgment (money, legal, public exposure, product taste, dropping
agent-proposed programmes). 45% a standing policy could answer; 9% were "did you
do X?" or "run this"; 6% access gaps; 4% duplicates or stale. Already fixed on
main and live: the merge desk's Fable-tier reviewer cards (53, `8b02157d3`) and
the steward's "did you fire it?" cards (14, `191d106ad`, live in 2026.306.2). The
largest remaining sources are plan gardeners asking about their own lifecycle
(42: mark complete, extend budget, pause) and merge-desk rule clashes (about 12).

## What stays with the owner

Money (purse overrides, paid services, the CI node cap, terraform applies that
cost or destroy), legal terms and identity, anything public, access for humans,
secrets and credentials (minting, rotating, exposing to a new principal),
GitHub PAT and App edits, changes to who may approve escalations, product
direction and taste check-ins, and dropping a programme an agent proposed. The
code floor (`escalationForcedReason`) stays as it is. The programme makes these
cheaper, not automatic: one daily "owner errands" list grouped by where the owner
has to be (P9.6).

## Owner rulings this programme needs (one decision card)

Six policy changes alter who decides. They are filed as **one** decision bead;
the tasks that depend on a ruling are blocked by it, and everything else starts
now.

1. **Plan completion.** A plan gardener completes its plan itself when the three
   close criteria hold and posts the evidence; the owner reopens if they
   disagree. Reverses ka-0000004595. (13 cards; owner chose "complete" 11/13.)
2. **Merge-desk sensitive paths.** The desk plus a second reviewer agent may
   merge a PR that references an already-approved credential class, seeds its
   own key, or only tightens authz. A credential exposed to a new principal, any
   loosening of who may approve, and the four-eyes stamp stay operator-merge.
   (21 cards; merged 17/19.)
3. **Thread-requested plans start approved.** A plan the owner asked for in a
   Slack thread is minted `building` with a "change or drop" notice instead of a
   blocking card. Plans agents propose themselves stay owner decisions (all 4
   drops were in that group).
4. **The release deployer runs.** Once P-R1..P-R3 land, unpause
   `gardenio-release-deployer` and stop re-suspending it after laptop rolls; the
   laptop rolls only floor-ordered or failed releases.
5. **Chart and agent-image edits are crew work.** Lift advice gd-oI2AIaSzjF for
   `gardenio/helm/**` and the agent image; the desk reviews them as high-risk
   paths; RBAC, cluster-scoped kinds and Secrets stay operator-merge.
6. **An escalation that carries out an owner ruling is born approved.** When an
   escalation names `implements_decision=<id>` and the server finds that
   decision closed by the owner with a chosen option the escalation cites, it is
   stamped approved by that decision (41 double asks).

## Phase P7 (routing plan): Execution never stalls

**P7.1 Hand-up re-targets execution.** In the idle hand-up sweep
(`kbeads/internal/server/escalation_policy.go`, the sweep that logs
"idle_handed_up"), when `held_by` moves to `operator`, re-stamp `execute_by` to
the first staffed lane that may execute the operation class: `coordinator:cluster`
for bead-only classes once Task 15 is live, else `operator`. This is a server
write through the existing route-stamp path, so `escalationFrozenFields` stays
closed to callers. Backfill: a one-shot `gb escalation reroute --approved-unexecuted`
(admin) that applies the same rule to the open set. Acceptance: an approved
escalation whose holder hands up is claimed by a lane within one sweep; test
covers hand-up of an approved bead-only item and of a floor item (stays
operator).

**P7.2 Dedupe at create.** In escalation create, refuse with 409 and the existing
id when an open escalation has the same project, the same normalised operation
type (P7.4) and the same `operation_spec` digest, or the same
`requesting_task`. The 409 body names the existing id so the filer comments
there. Same for decisions: same filer lineage and same prompt digest while open.
Acceptance: unit tests for both; the three checks:read asks would have been one.

**P7.3 Dispatch checks the executor can do it.** Before
`startEscalationExecutor` (`escalation_targets.go`) spawns a pod for an approved
item, refuse dispatch when `execution_target=local`, or the op class needs an
identity the pod role lacks (kubeconfig, AWS, admin login), and route it to the
lane named by P7.1 instead. Acceptance: none of the 16 bounce shapes
(gd-OzKA0KptyC, gd-p57IcSTGIZ) reproduces in a test.

**P7.4 A closed set of operation types.** Add an `operation_type` registry (the
126 seen values folded to about 25 classes, with aliases) in kbeads; create
normalises aliases and, in warn mode, logs unknown types; enforce after one
release with the count at zero. Policy rules match on the class. Acceptance:
the policy's catch-all rule matches under 10% of new escalations for a week.

**P7.5 Owner rulings approve their own execution** (needs ruling 6).
`implements_decision` field on escalation create; the server verifies the
decision is closed, responded by an OIDC owner, and that the chosen option id
appears in the escalation's `operation_spec.chosen`; then stamps
`approval_method=decision`, `approved_by=<decision responder>`. Floor items are
excluded. Acceptance: tests; the budget-raise-after-"extend" shape needs no
second click.

**P7.6 Drain the backlog (operator, now).** The laptop executes or closes the 64
approved-unexecuted escalations: closes the ones a live release already carries
(verify the merge commit is in the live tag), executes the rest it may, hands
floor items to the errands list (P9.6). Acceptance: approved-unexecuted count 0.

## Phase P8 (routing plan): Agents act and tell, not ask

**P8.1 Plan gardeners complete their plans** (needs ruling 1). Replace the
"FILE AN OPERATOR DECISION proposing closure" block in
`gardenio/controller/internal/plangardener/plangardener.go` (step 1c, close
criteria) with: verify the three criteria, comment the evidence on the plan, run
`gb plan complete <plan>`, post one line to the plan's channel, `gb stop`. Update
`plangardener_test.go:310`. Acceptance: test; the next plan to meet its criteria
completes with no card.

**P8.2 No budget asks.** In `AppendExitGuidance` (`plangardener.go:307`) replace
"Run budget: at most %d runs before you should wrap up the loop" with "The engine
re-arms your budget; never ask to extend it. Ask only if the engine labels your
trigger gardener-stuck-escalate." Acceptance: test asserts the text; no
"extend budget?" card for a week.

**P8.4 Shared advice: never ask whether something was done.** Add the steward's
rule (`191d106ad`) to the fleet working-rules advice: never file a decision asking
the owner to run a command or whether they did something; label a step that
needs the owner at a device `owner-at-device`, list it once in the errands digest
(P9.6), and close it when the evidence shows it done. Batch a spec's questions
into one card. Acceptance: advice bead updated (operator, admin write); a week's
decisions have zero C-class cards.

**P8.5 Merge desk: owner CI rules win, no card.** Add to `docs/merge-desk.md`:
when a desk step would break an owner CI rule (no big PR runs, main is the proof,
quarantine failing tests, no bake periods), take the cheapest compliant path,
note it on the PR, file nothing. Operator-merge PR (it changes the merge
policy). Acceptance: no rule-clash card for a week.

**P8.5b Merge desk: split the OPERATOR-ONLY rule** (needs ruling 2). Rewrite
`docs/merge-desk.md:720-733` to ruling 2: the desk plus a second reviewer agent
merges PRs that reference an already-approved credential class, seed their own
key, or only tighten authz; a credential exposed to a new principal, any
loosening of who may approve, and the four-eyes stamp stay operator-merge.
Operator-merge PR.

**P8.6 Desk prompt installs on merge.** A merged change to `docs/merge-desk.md`
is the approval: the release deployer (or the laptop, until P-R4) installs the
prompt from the merged file onto trigger gd-WksO1FSmvV and reseals it, recording
the commit on the trigger. One click instead of two; no v17/v17a/v17b pile-ups.

**P8.7 Thread-requested plans start approved** (needs ruling 3). The thread
agent's plan mint (`gb plan mint`) sets `plan_state=building` and binds the
gardener when the Slack thread's human author is the home garden's operator and
asked for the plan in that thread; it posts a "change or drop" notice instead of
a decision. Acceptance: test; next thread plan builds with no card.

## Phase P9 (routing plan): Standing access instead of escalations

**P9.1 GitHub App installation tokens for crews and the reviewer.** After the
owner's one-time App cutover (gd-gCzUTAg2a1: Checks read, Actions write, Commit
statuses read, all fleet repos), crews and the reviewer get installation tokens
instead of the fine-grained PATs, and `gb ci rerun <run>` / `gb ci dispatch
<workflow> --ref <branch>` wrap the Actions API with an allowlist (ci.yml,
checks.yml, images.yml dispatch only on agent branches) and a per-PR rate limit
(2 per hour). Actions: write cannot edit workflow files. Retires the 15 open
token-scope escalations and the empty-commit full-CI re-runs. Owner part: the
App permissions click only.

**P9.2 A read-only verification identity for crews.** A `verifier` capability
on crew and infra-reader roles: a viewer-allowlisted service-account token for
GET pages (gd-sebFn2kYFm, gd-FqYfKt82or), read-only kubectl without
`secrets` (no `get secrets`, no `helm get manifest`), and the land-owner test
identity (gd-n06sDdZ7yq). Egress allowlist unchanged. Acceptance: a crew runs
the gd-YCpjRdp4N1 curls and the gd-XLra7iK3XE 302 check itself.

**P9.3 Chart and agent-image edits in the crew lane** (needs ruling 5). Update
advice gd-oI2AIaSzjF; `docs/merge-desk.md:709` already treats `gardenio/helm/**`
and `deploy-values/**` as high-risk paths that need evidence. Operator-merge stays
for RBAC, cluster-scoped kinds and Secrets. Retires gd-NkeZ5hbdJ4,
gd-XBQQookt6R, gd-aUIR1KXC1B, gd-APoJVXk6R4, gd-Hv60lq5XOX, gd-LrfaWsHbU9 as crew
tasks.

**P9.4 infra-reader reads AWS cost and metrics.** Add Cost Explorer read and
CloudWatch read (GetMetricData, ListMetrics, logs FilterLogEvents on the
gardenio log groups) to the infra-reader IAM role in terraform. Owner approves
the plan diff (terraform stays owner-applied).

**P9.5 Bead-only work goes to the cluster lane.** Task 15 (in flight) is the
executor; this step adds the policy rule: admin-only writes to non-config beads
inside an owner-approved plan (tunables, release records, heraldry, plan
lifecycle) are approvable by `coordinator:cluster`; advice, role,
spawn_policy and escalation_policy writes stay operator. Operator-merge (policy).
*Status 11-05:* the owner's floor ruling (gd-CJUlVSMzEe, P10.F) replaced this rule
with D7 rule 4: classes `bead_admin`, `bead_close` and `advice` are held, approved and
executed by `coordinator:cluster` in any plan or none, and role, spawn_policy and
escalation_policy writes are on the floor in code. P9.5 adds what rule 4 lacked: the
executor grant accepts every type it routes there (plan lifecycle, dependency edits).

**P9.6 One owner errands list.** The steward's daily digest gets a "what you
must do" section: every open owner-only item (floor escalations,
`owner-at-device` steps, owner decisions) grouped by place (GitHub settings,
Slack admin, Keycloak, AWS console, phone), each with the exact clicks and the
evidence that closes it. Nothing owner-only is filed twice; the digest is where
it waits.

## Phase P-G (grants plan): Dispatched-task grant and gardener self-idle

**P-G1 Crews may finish the task they were dispatched on.** Add a grant row `dispatched-task` to the table in
`kbeads/internal/server/ownership_grants.go`: an agent whose agent bead's
`task_id` is bead B may claim, update, close and add dependency edges on B and on
beads B lists as fold-ins. At spawn with `--task`, stamp the task's assignee to
the agent when it is empty. Retires cluster B (85 escalations, gd-lEoaASsJRl,
gd-7i4N2QYfon, gd-Ed1EVvC6xs, gd-ZeEdW4ViTe) and answers decision
gd-pjrfJOLdiY's cause. Tests: own task allowed; another agent's task denied; a
closed agent's task denied.

**P-G2 A gardener may idle its own trigger on a named event.** A grant row: the bound gardener of a plan may
write `enabled`, `cron` and `wake_on` on its own trigger (the trigger whose
`gardener-epic:<plan>` label matches the plan in its agent bead) when the update
also sets `awaiting=<bead id or event>`; the engine re-enables it when that
bead closes. Replace "Only the operator disables a gardener trigger" in
`AppendExitGuidance`. The server reseals through `stampSpawnLineageFromPlanApproval`
so the trigger keeps firing. Acceptance: tests for own trigger (allowed), another
plan's trigger (denied), no `awaiting` (denied).

## Phase P-R (releases plan): Releases ship themselves

**P-R1 Values drift parks only on hand edits.** In
`gardenio/scripts/release-deploy.sh` rule 3b (line 380), compare live values
with the overlay of the **previously deployed** tag, not the new one: equal means
nobody hand-edited, so apply the new tag's reviewed overlay; different means a
hand edit, so park and file one escalation as today. Cluster-scoped additions
beyond the deployer's ClusterRole still park. Test in the script's test harness.

**P-R2 Edge manifests ride the release.** `release-deploy.sh` server-side applies
`gardenio-ziti/public-edge.yaml` and `viewer-authproxy.yaml` from the tag beside
rule 4b, then `nginx -t` and reload, and rolls back the ConfigMap on a failed
test. Retires the "apply public-edge.yaml after #N" escalations.

**P-R3 Merged work closes when it ships.** A task field `awaits_release=true`
(set by a crew instead of filing "roll PR #N"); the deployment record
(`POST /v1/releases/<tag>/deployments`) closes every open task and escalation
whose merge commit is an ancestor of the deployed tag. Change `CLAUDE.md`
("files a `helm_deploy` escalation that names the next window") to the new field.

**P-R4 Turn the deployer on** (needs ruling 4). Delete the
`release-deployer-pause` ConfigMap (paused by the laptop since 10-21), stop
re-suspending after laptop rolls, and update the operating notes. The laptop rolls
only floor-ordered releases (security sequences such as #1686 then
gd-gdf3L8kjT5) and failed ones.

## Delivery: how we make sure it gets done

1. **Every step is a bead in a building plan with a live gardener.** No step
   lives only in this document. The gardeners dispatch crews as steps unblock;
   the steward's quiet-gardener sweep wakes a gardener that goes quiet.
2. **One tracker bead** lists every step id with its state and the weekly
   numbers below. The laptop refreshes it each day at the ~16:00Z window and
   comments the delta.
3. **Numbers, measured daily** by `access-metrics.sh` (laptop skill scripts),
   from kd:
   - non-merge escalations reaching the owner per day (target ≤ 5, from ~25),
   - owner decisions filed per day (target ≤ 10, from ~27),
   - approved-but-unexecuted escalations (target 0, from 64) and the oldest age,
   - programme steps closed / total.
4. **Blockers surface at once.** Each laptop tick lists programme steps blocked
   on an owner or operator item. Operator items the laptop does in that tick.
   Owner items go on the errands list and one push notification per newly
   blocked state.
5. **Done means measured.** The programme closes when every step is closed and
   the first three numbers hold their targets for 7 consecutive days.
