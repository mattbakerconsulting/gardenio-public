# Escalation Request Runbook (Agent Side)

**Audience:** agents that need a privileged operator to perform an action they
cannot perform themselves (AWS IAM, K8s RBAC, secret rotation, GitLab admin,
Vault provisioning, etc.).

**Sibling docs:**
- `gardenio/docs/archive/escalation-infrastructure-report-2026-04-06.md` —
  implementation status, code inventory, architecture diagram (V1, archived).
- `gardenio/plans/archive/privilege-escalation-pipeline.md` — V1 design (archived).
- `gardenio/plans/escalation-v2-agent-driven-pipeline.md` — V2 schema (executable
  scripts, credential requirements, sensitivity-classified outputs).

---

## When to use `gb escalation create`

Use it when **all** of these are true:

1. The work needs an action you (the agent) cannot perform — typically because
   the gardenio-agent service account lacks the privilege, or the action lives in
   a system gardenio is not connected to (a non-monorepo Terraform repo with its
   own CI, an upstream SaaS console, a human-only IdP, etc.).
2. The action has a **concrete operation spec** — you can describe target,
   parameters, and expected outcome precisely. (If you can't, you don't need an
   escalation yet — you need more research.)
3. The action is **production-impacting or auditable** — it touches infra,
   credentials, RBAC, or anything that benefits from a paper trail of who
   approved what, when, and what came back.

Use a **decision** (`gb decision create`) instead when you need a human's
*judgment* on a fork (which approach, which option, accept/revise/defer). Use
**squawk** when you just need to report progress or ask a casual question.

A useful litmus test: *if a privileged operator opened a terminal right now
and you handed them this thing, could they execute it without writing more
specification?* If yes, it is an escalation. If no, it is still a decision (or
a research bead).

## Required fields

`gb escalation create` requires:

- `--title` — one-line, action-shaped (`Grant ec2:DeleteVolume on
  gardenio-agents-s3`).
- `--operation-type` — one of `aws_iam`, `k8s_rbac`, `credential_rotation`,
  `credential_provisioning`, `vault_provisioning`, `infrastructure`,
  `infrastructure_change`, `configuration`, `ticket_management`,
  `manual_intervention`, `general` — pick the closest. New types are fine if
  none fit; this is a free string.
- `--operation-spec` — a JSON blob the operator (or service account) parses to
  carry out the work. Include enough that the operator does not have to read
  prose to act:

  ```json
  {
    "actions": ["ec2:DescribeVolumes", "ec2:DeleteVolume"],
    "resource_tag_condition": {
      "aws:ResourceTag/kubernetes.io/cluster/america-e2e-eks": "owned"
    },
    "role_arn": "arn:aws:iam::234567890123:role/gardenio-agents-s3",
    "terraform_repo": "site-deployment"
  }
  ```

You **should** also provide:

- `--script` (or `--script-file`) — an executable script. Use `--script manual`
  when the work genuinely requires human judgment that cannot be scripted (UI
  click in a SaaS console, multi-system coordination). A `manual` script
  signals to the operator: "no automation, you are the executor."
- `--context` — what you tried, what failed, and why this requires elevation.
  Link to the requesting task and any related research/decisions.
- `--credentials` — JSON array describing what access the operator will need
  (AWS role ARN, kubeconfig, GitLab admin, etc.). Saves the operator a
  guessing pass.
- `--expected-outputs` — JSON array, each entry classifying sensitivity
  (`public|internal|sensitive|secret`) and format. Anything sensitive **must**
  be classified — results are visible in the escalation bead, and unclassified
  secrets leak into Slack and agent logs.
- `--sensitivity` — overall classification (`public|internal|sensitive|secret`).
- `--audience owner` — send the escalation to the owner. The daemon decides
  who an escalation is for when you file it: `desk` (the merge desk) for
  `gitlab_mr_merge`, `operator` (infra) for every other type, unknown ones
  included, and `owner` for one that needs `--credentials`, is
  `--sensitivity secret`, names an approver, or needs an admin to approve.
  `owner` is the only value you can ask for. Only owner-audience escalations
  show in full on the owner's Commons and beads-viewer and ping the owner;
  desk and operator ones fold into one "In hand: N (desk X, operator Y)" line
  and go to their own channels. Say `--audience owner` for a product or policy
  choice, anything only the owner can do (passkey/MFA, GitHub sudo, legal), or
  spending money. Audience, sensitivity and credentials are fixed once filed:
  no field write changes them. Audience never changes who may approve. An
  operator or admin, signed in as themselves, re-routes one with
  `gb escalation route <id> --audience owner|operator|desk` or from the
  escalation's page in beads-viewer; both use
  `POST /v1/escalations/{id}/audience`, which records a comment and keeps a
  forced escalation with the owner (gd-bmOiteQi36).
- `--requesting-task` — the task bead you are working on. Without this, the
  bridge cannot nudge you when the escalation closes. When set, create also
  writes a `blocks` dependency edge from that task to the escalation, so the
  blocked work — and, when the task is a plan step, its owning /plans plan —
  visibly reads as blocked by the escalation until it closes. Escalate *from*
  the bead you are actually working so the edge lands on the right work item.
- `--close-on-execute` — opt into the **close-on-escalation-execute contract**
  (feature ka-0000001587). Writes `close_requesting_task=true` on *this*
  escalation (without the flag it writes `false`); when the escalation closes
  `success`, the daemon closes `--requesting-task`. Use it ONLY when the
  requesting task's remaining work IS the escalation: a merge escalation for the
  task's own PR, or a wrapper step such as "Escalation: operator seeds
  observability creds". Do **not** use it for real work that merely happens to
  be blocked by an escalation, since that work has its own follow-up after the
  blocker clears. The opt-in lives on the escalation, so it never carries over
  to a later escalation against the same task (gd-91b7zMOR2z); the old
  `close-on-escalation-execute` task label is read only for escalations filed
  before that field existed.
  - If the `blocks` edge could not be written (you do not own the task; create
    prints a WARNING), the task still closes, but only when whoever resolves the
    escalation may close that task by hand. Otherwise the daemon comments on the
    task instead (gd-MgicCcY2rj).
  - A resolver whose outcome does not finish the task (the merge did not carry
    its fix) closes with `gb escalation close <id> --status success
    --keep-requesting-task`, which withdraws the opt-in in the same write. Never
    close first and remove the opt-in after: the auto-close runs inside the
    close and wins that race (gd-Ma2u0JfCFE). A result carrying
    `"merged": false` closes nothing either.

Run `gb escalation create --help` for the full flag set and a worked example.

## Lifecycle (what happens after you create)

1. Bridge posts a Slack notification card with priority emoji + a "Review in
   Web UI" button. Slack is **notification-only** — approval cannot happen
   from Slack.
2. An operator with the `approve:escalation` capability reviews the bead in
   the OIDC-authenticated web UI (`/escalations`).
3. On approve:
   - `local_claude` → spawns a fresh Claude Code agent on the operator's
     laptop with the script as prompt.
   - `local_gardenio` → nudges an existing local `gb agent start --local` to
     execute, falling back to spawn.
   - `service_account` → not implemented yet (records failure today).
4. The execution result is written back to the escalation bead and the
   requesting agent (you) is nudged via mail.

Until the bead closes, you are blocked on the operator. Either `gb stop` (and
get nudged on closure) or `gb yield` against an explicit decision if you have
one open. Do **not** loop polling the escalation status — that burns context
and the bridge will wake you when it closes.

### Resolving an escalation (resolver side)

An escalation is a shared operator work item, so any operator — not just the
filer — may resolve it. But it is resolved through its **dedicated verbs**, not
`kd close`:

| Intent | Verb | Endpoint | operation_spec replay |
|---|---|---|---|
| Approve + execute | `gb escalation approve <id>` | `POST /v1/escalations/{id}/approve` | **Yes** — the daemon auto-replays the denied create/update/close op (machine-operator context), records the result, and closes the bead. |
| Reject / not needed | `gb escalation reject <id> --reason …` | `POST /v1/escalations/{id}/reject` | No — closes as `result_status=failure`; no side effect runs. |
| Record an externally-run result | `gb escalation close <id> --status … --result …` | `POST /v1/escalations/{id}/result` | No — records the result you supply and closes; success releases close-on-escalation wrappers. |

A plain `kd close` (or the viewer's generic Close button, or gRPC `CloseBead`)
on an **open** escalation is **refused** (`409` / `FailedPrecondition`) and
points you at these verbs (ka-0000005516). This is deliberate: a generic close
would skip `operation_spec` replay and would still fire the auto-close cascade,
releasing `close-on-escalation-execute` wrapper tasks as though the privileged
operation had run — a silent no-op. Use `reject` to close an escalation that
should not execute.

## Stalled escalations: pingback, reflag, and the automated cadence

The engine owns re-notification (ka-0000004385) — you do **not** need to
hand-roll Slack nudges for a stalled escalation:

- The slack-bridge auto-re-posts stalled, unapproved escalations at **~12h**
  and **~24h** after the original card, alerts the named operator queue
  channel past **~48h**, then keeps re-notifying with doubling backoff
  (96h, 192h, ...). All cadence state lives on the bead
  (`auto_pingback_count`, `pingback_*`, `queue_alert_*` fields), so it
  survives bridge restarts.
- `gb escalation pingback <id> [--reason]` — manual re-surface **ahead** of
  the automated cadence, without changing priority. Rate-limited to one per
  hour per escalation (`--force` overrides). The re-post lands as a broadcast
  reply on the original Slack card.
- `gb escalation reflag <id>` — when the escalation got more **urgent**, not
  just stale: bumps priority one step toward P0 and also triggers the Slack
  re-post.

`gb escalation show <id>` renders the pingback state (count, last source,
last posted, queue-alert time) under the Pingbacks section.

### Owner-of-last-resort: the `successor` fallback

Pingback/reflag re-surface an item to **the same operator**. When that operator
is offline entirely, an item can stall with nobody minding it. Give the item a
**successor** — a fallback human (a `type=user` bead) who should pick it up:

- `gb escalation successor set <id> <user-bead-id>` — stamp the fallback owner
  (`--clear` to remove). The target must be a `type=user` bead.
- `gb escalation successor resolve [--idle 1h] [--dry-run]` — re-route every open
  item whose owner has been idle past the threshold to its successor.

`resolve` **only re-routes** — it never closes, decides, approves, or rejects on
anyone's behalf (the cross-operator guard would refuse that anyway). The same
`successor` verbs exist under `gb decision`. Full reference:
`gardenio/docs/decision-queue-owner.md`.

## Anti-patterns

- **Decision masquerading as escalation.** "Should we widen this IAM policy or
  not?" is a decision. "Apply this exact widening to this exact role" is an
  escalation. Surfacing the choice as an escalation forces the operator to
  pick AND act in one step, which is the wrong shape for a judgment call.
- **Escalation without a spec.** "Please give the agent more permissions" is
  not actionable. Name the role, the actions, the resource conditions.
- **Unclassified outputs.** If the operator runs your script and the result is
  a fresh DB password, mark `expected_outputs[].sensitivity = "secret"`.
  Otherwise it lands in plaintext on the escalation bead and gets quoted in
  Slack.
- **No requesting task.** Orphaned escalations have no agent to nudge on
  completion, and no `blocks` edge is written — so the blocked work (and its
  /plans plan) never shows the blocked-by-escalation signal. Always pass
  `--requesting-task`.
- **Polling instead of yielding.** After creating an escalation, `gb stop`.
  The bridge nudges you on closure. Don't burn cycles checking
  `gb escalation show`.
- **Ad-hoc Slack nudge spam for a stalled escalation.** The engine already
  re-notifies at 12h/24h/48h with backoff and alerts the operator queue past
  48h. If it is urgent sooner, one `gb escalation pingback` (or `reflag` if
  the priority genuinely rose) — not a hand-rolled nudge loop.

## Backfilling links for older escalations

Escalations created before the blocks-edge landed (2026-07) carry only the
`requesting_task` string field. `gb escalation backfill-links` materializes
the missing `blocks` edges for the current open queue (idempotent, capped at
200, `--dry-run` to preview) so the /plans blocked-by-escalation signal lights
up for pre-existing escalations too.

Escalations filed before the `audience` field (2026-10) read through the same
defaults, so nothing is lost; `gb escalation backfill-audience` (`--dry-run`
to preview) has the daemon stamp the field on the open queue so it matches.
It goes through the re-route endpoint, which only a gardenio-operator or admin
may use: run it signed in as one.

## Worked example

Agent realizes the EBS GC loop on prod gardenio-agents needs `ec2:DescribeVolumes`
and `ec2:DeleteVolume` to actually reap orphaned volumes. The IAM policy lives
in a separate Terraform repo (site-deployment) the agent cannot push to.

```bash
gb escalation create \
  --title "Grant ec2:DescribeVolumes + ec2:DeleteVolume on gardenio-agents-s3 IAM role" \
  --operation-type aws_iam \
  --operation-spec '{
    "actions": ["ec2:DescribeVolumes", "ec2:DeleteVolume"],
    "resource_tag_condition": {
      "aws:ResourceTag/kubernetes.io/cluster/america-e2e-eks": "owned"
    },
    "role_arn": "arn:aws:iam::234567890123:role/gardenio-agents-s3",
    "terraform_repo": "site-deployment"
  }' \
  --script manual \
  --context "EBS GC loop is enabled in prod (commit 1283fc84) but the controller SA role only has S3. ec2:DescribeVolumes returns UnauthorizedOperation; orphan EBS volumes accumulate. IAM policy lives in site-deployment Terraform — agent cannot push there." \
  --credentials '[{"type":"aws_role","identifier":"site-deployment maintainer","purpose":"Apply Terraform diff"}]' \
  --sensitivity internal \
  --requesting-task ka-0000006717
```

(This is escalation `ka-0000001326` in the live system — open as of 2026-04-29.)

---

## Proposed global advice text

Designed to be short and memorable — long-form guidance lives in
`gb escalation create --help` (which now embeds the WHEN/WHAT/LIFECYCLE/
ANTI-PATTERNS sections) and in this doc. Target body length: ~200 tokens.

> **Title:** Use `gb escalation create` to ask a privileged operator to do something — not `gb decision`
>
> **Body:**
>
> When you need a human (or system) with elevated privileges to perform a
> concrete, audit-worthy action you cannot do yourself — AWS IAM, K8s RBAC,
> secret rotation, GitLab admin, Vault provisioning — use `gb escalation
> create`. Not a decision, not a squawk.
>
> Litmus test: *could a privileged operator open a terminal and execute this
> right now without more spec from you?* Yes → escalation. No → write a
> `gb decision` first to lock the spec.
>
> Required: `--title`, `--operation-type`, `--operation-spec` (JSON, not
> prose), `--requesting-task`. Strongly preferred: `--script` (or `--script
> manual`), `--context`, `--credentials`, `--expected-outputs` with per-output
> `sensitivity` (REQUIRED for secrets — unclassified secrets leak into Slack),
> `--sensitivity`. **Run `gb escalation create --help`** for the full flag
> reference, lifecycle, worked examples, and anti-patterns.
>
> After you create: `gb stop`. The bridge nudges you when the operator closes
> the bead. Do not poll.
