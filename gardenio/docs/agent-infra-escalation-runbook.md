# Escalating to an infra agent (crew runbook)

Task gd-fHpSoNm2Ky (plan gd-xiV5hyhPrG, step P7). Written from the live check in P3
(gd-QlOI7v7Noa, 2026-10-14). Every command below ran in P3 unless it is marked
**not yet live**.

**Audience:** a crew or plan-captain agent in project `gardenio` that needs to see
something in the cluster (nodes, pods, events, pod logs) and has no `kubectl`.

Your pod has no ServiceAccount token, so `kubectl` fails with "no usable kubeconfig"
(see `agent-beads-viewer-access.md`, row 18). You do not need an operator to get one
looked at. You ask for an **infra agent**, which is a short-lived second agent that
holds a read-only cluster token, and read its answer on your task bead.

## Which tier do I need?

| I need to… | Use | Approval |
|---|---|---|
| Read nodes, pods, events, workloads, storage; read pod logs | `infra` | **Automatic**, within caps (below) |
| Read beads, look at the beads-viewer | nothing; `kd`/`gb` work already | none |
| Change anything in the cluster (apply, scale, restart, RBAC) | `infra-admin` | A human, every time |
| Work inside a project's own namespace (ClaimLayer first) | `<project>-infra` | **not yet live**, see the last section |
| A secret value, `exec` into a pod, or any AWS action | not available to `infra`; file an escalation (`escalation-request-runbook.md`) | A human |

Prefer the narrowest tier that answers your question. `infra-admin` does not replace
`infra` (`infra-admin-role-design.md` §5).

## The command

```bash
gb spawn --role infra --escalate --prompt "<what to read, what to report, where>"
```

- `--role infra` names the read-only role. It is **privileged**, so a plain
  `gb spawn --role infra` is denied for a crew caller. `--escalate` turns that denial
  into an escalation instead of an error.
- Success prints
  `gb spawn --escalate: role "infra" is privileged; filed escalation <esc-id> to spawn agent "<name>" on project "gardenio". Waiting on operator approval or an auto-approving spawn_policy rule.`
  Note the escalation id.
- Do **not** pass `--task <your-own-task>`. P3 observed that
  `gb spawn --task <id>` where you already hold `<id>` returns `status=existing`
  (reason `single_claim`) and spawns nothing. Name the bead in the prompt instead.
- Run it from your own pod. `--escalate` is for crew and other non-human callers.

## What happens next, and how long it takes

1. The escalation is evaluated the moment it is filed against the `spawn_policy: infra`
   bead (gd-V6e5WM4YUG). In P3 the first request was approved and the agent was
   created in the same second.
2. If the request is over a cap it is not rejected. It **stays open** and a sweep
   re-examines it every **30 seconds** (`spawnPolicySweepInterval`). It is approved on
   the first tick after a slot frees, or the day rolls over. Otherwise it waits for a
   human.
3. An approved request shows `approved_by=system:spawn-policy`,
   `approval_method=policy`, `policy_rule=infra#0` (crew) or `infra#1` (plan-captain),
   and the new agent bead has `spawn_approval_mode=auto` and `spawned_by=<esc-id>`.
4. The infra agent runs your prompt and comments on the bead. In P3 a three-command
   read finished in about one minute.

Check state without waiting on anyone:

```bash
kd show <esc-id> --json | jq '{status, approved_by, approval_method, policy_rule}'
```

### Caps

The policy bead has two auto rules, one for requester role `crew` and one for
`plan-captain`, both in project `gardenio`:

| Cap | Value | What it counts |
|---|---|---|
| `max_per_day` | 6 | infra spawns approved under that rule in a day |
| `max_concurrent` | 2 | infra agents **alive right now** |

A third rule sends everything else (another project, another requester role) to a
human. D1 revisit date: **2026-10-27**.

The concurrent cap counts live agents, not agents ever started. P3's first over-cap
attempt was invalid for that reason: the first infra agent had already finished, so
the "third" request was auto-approved. A fast infra agent frees its slot in under a
minute. With two infra agents live, a third request stayed open for 73 s (two sweep
ticks) with no approver, no policy rule and no agent bead, which is the intended
"waits for a human" behaviour.

If you are over cap, do not file more requests. They add nothing. Wait for a slot
(the sweep approves you) or for a human.

## What the infra agent can and cannot do

Measured in P3 from the infra agent's own `kubectl auth can-i --list`, and equal to the
ClusterRole in `gardenio/helm/gardenio/templates/controller/infra-reader-rbac.yaml`:

| Can | Cannot |
|---|---|
| `get`/`list`/`watch` pods, nodes, events, configmaps, services, endpoints, PV/PVC | read **Secrets** (not in the list) |
| Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, CronJobs | `exec`, `attach`, `port-forward` |
| `pods/log` (read container logs) | create, update, patch or delete anything |
| pod and node metrics (`get`/`list`) | |

The only `create` verbs it holds are the self-review APIs (`selfsubjectreviews`,
`selfsubjectaccessreviews`, `selfsubjectrulesreviews`), which every authenticated
identity gets by default.

It runs in project `gardenio`, as ServiceAccount `gardenio-infra-reader`, with a
500m/1Gi request and 1500m/2Gi limit. It has no `aws` CLI and no AWS grants from this
role. Its output is a comment on a bead, so **do not ask it to print Secret values,
tokens or kubeconfigs**, and tell it not to print log contents it does not need.

Events are only kept for a limited time (P5, gd-AJDziONiXG). If you need history, say
so in the prompt and ask the infra agent to report what it could not see.

## How to write the prompt

The infra agent starts with nothing but your prompt and the role description. It cannot
see your conversation. The working example is gd-ozKvDV1FqD (the capacity
investigation). The P3 prompt is the minimal pattern:

```bash
gb spawn --role infra --escalate --prompt "Read-only check for gd-XXXXXXXXXX: \
run 'kubectl get nodes', \
'kubectl -n gardenio logs deploy/gardenio-beads --tail=20 | wc -l', \
and 'kubectl auth can-i --list'; \
comment a summary of the results (no log contents, no secret values) on gd-XXXXXXXXXX; \
then gb stop."
```

Put these in every prompt:

1. **The bead id** to report on, and "comment on it". The comment is how you get the
   answer back.
2. **The exact read-only commands**, or the exact question if you do not know the
   commands. Give namespaces, resource names and a `--tail` or `--since` limit.
3. **What to leave out.** "No log contents, no secret values." Ask for counts,
   names and timestamps, not dumps.
4. **A stop condition.** End with "then `gb stop`". An infra agent that keeps going
   holds one of your two concurrent slots.
5. **Read-only framing.** Say "do not change anything". If the fix needs a write, the
   agent should write it up in the comment (as a diff or a command), not run it.

Keep one escalation to one question. Two small asks that fit in one prompt are one
request; two unrelated investigations are two, and the daily cap is 6.

## When to yield and when to stop

- **`gb stop`** is the default. Do the other work you can, comment on your task with the
  escalation id and what you asked, and stop. Closing the escalation is what wakes you,
  per the fleet convention (see the plan's lane rules). Do not sit in a live pod
  polling.
- **Never** start a background watcher and end your turn to wait for it. Ending the turn
  with nothing running ends the pod (`gb advice show gd-L4MSFKLoV5`).
- If you must have the answer to finish this turn and the auto path ran, wait in the
  **foreground** for a minute or two, then re-read the bead:
  `kd show <your-task> | tail -20`. P3's agent commented within about 45 seconds of the
  spawn. If the escalation is still open with no `approved_by` after two minutes, you
  are over cap or outside the policy. Stop and let a human approve.
- `gb yield` is for a `gb decision` you need answered in this turn. It is not the way to
  wait for an infra agent. An escalation is not a decision.

## Closing an escalation you filed by mistake

`kd close <esc-id>` is refused for escalations (HTTP 409). Use:

```bash
gb escalation close <esc-id> --status failure --result 'filed in error'
```

## `infra-admin` (full cluster control; human approval only)

Use it only for a platform **change** that `infra` cannot make: apply, scale, restart,
RBAC, a CRD. Design: `infra-admin-role-design.md`.

- It is bound to `cluster-admin`. A compromised pod is a compromised cluster, so it is
  **never auto-approved**. No `spawn_policy` auto rule can exist for it
  (`neverAutoApproveRoles` in `kbeads/internal/server/spawn_policy.go`), and every spawn
  needs an OIDC human to approve it from a browser or Slack session (not from your pod).
- The request has the same shape, with a different role:
  `gb spawn --role infra-admin --escalate --prompt "..."`. Then **stop**. Nobody will
  approve it in your session.
- State in the prompt exactly what to change, why `infra` is not enough, and how to
  undo it. The agent is told to comment every mutating command on its task bead.
- **Status 2026-10-14: not yet usable.** The chart and the kbeads guard are merged
  (`agents.infraAdmin`, PR #340), but the `infra-admin` role bead and its
  `spawn_policy` bead do not exist yet; the operator writes those. Until P13
  (gd-Pu4KI4mLXk) closes, a request for this role will not produce an agent. Check
  `kd list -t role` for a role named `infra-admin` before you rely on it.
- If you only need to read, use `infra`. If you need a change and `infra-admin` is not
  live, write the change as a PR or a diff in a bead comment and file a
  `gb escalation create` for the operator.

## Per-project infra roles (`claimlayer-infra`)

<!-- STUB: P9 (gd-IYY1dxukfR) fills this section when it closes. -->

**Not yet live (2026-10-14).** The design is `project-infra-role-design.md` (P8). The
chart half of the first instance, `claimlayer-infra`, is in draft PR #346 and has not
been merged or rolled. No `claimlayer-infra` role bead exists.

When P9 closes, this section should say: the role name, the namespace it controls
(`claimlayer-demo`), the exact `gb spawn --role claimlayer-infra --escalate` line, which
requesters are auto-approved, and what it must escalate up to `infra-admin` (shared
platform pieces such as Keycloak).

## Quick reference

```bash
# 1. ask for a read-only look (auto-approved within caps)
gb spawn --role infra --escalate --prompt "Read-only check for <bead>: <commands>; comment a summary on <bead> (no secrets); then gb stop."

# 2. see what happened to the request
kd show <esc-id> --json | jq '{status, approved_by, approval_method, policy_rule}'

# 3. read the answer
kd show <bead> | tail -20

# 4. withdraw a request you do not need
gb escalation close <esc-id> --status failure --result 'not needed'
```

Evidence for every number above: comments on gd-QlOI7v7Noa (P3). Policy:
gd-V6e5WM4YUG. Role: gd-VINFcACohF.
