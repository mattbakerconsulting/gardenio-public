# Escalation review loop: as-is (2026-09-20)

As-is baseline for epic gd-3UQKfj0nA8 ("Agent PR merges via escalation to an
internal reviewer role on a stronger model"), task gd-ntQFh1cPb0. Scope: the
escalation lifecycle and authz, the replay executor, the `type:role` model
override chain + LiteLLM model/pricing, how a crew blocks/resumes on an
escalation today, and a baseline table of every `github_pr_merge` escalation
in the last 3 days. All citations are `file:line` against main @ f44293c
unless noted.

## 1. Escalation lifecycle and authz

One underlying mechanism, two CLI fronts. `kd escalation` (kbeads/cmd/kd/escalation.go)
and `gb escalation` (gardenio/controller/cmd/gb/escalation*.go) both target the
same HTTP surface — `/v1/escalations/{id}[/approve|reject|result]` — against
the single `model.TypeEscalation` bead kind
(kbeads/internal/server/http_escalations.go:54), via two parallel client
wrappers (kbeads/internal/client/escalations.go,
gardenio/controller/internal/beadsapi/client_escalations.go). `gb escalation`
is the agent-facing wrapper (adds create/reflag/pingback/successor/backfill-links);
`kd escalation`'s own `--help` documents it as the thinner operator-facing
CLI hitting "RoleOperator-gated" endpoints.

Route table (kbeads/internal/server/http.go:122-193):

| Verb | Route | Role gate | Extra identity check |
|---|---|---|---|
| create | `POST /v1/beads` (generic, `type=escalation`) | `RoleOperator` (http.go:160) | none — **any agent with a write-capable bearer token** |
| get/list | `GET /v1/escalations[/{id}]` | `RoleViewer` (http.go:122-123) | cross-project read scope (`authorizeBeadRead`) |
| approve | `POST /v1/escalations/{id}/approve` | `RoleOperator` (http.go:191) | **`escalationApprover` requires an OIDC-derived human identity** (http_escalations.go:147-151) + four-eyes (`escalationRequiresFourEyes`, http_escalations.go:159-162): approver must differ from requester |
| reject | `POST /v1/escalations/{id}/reject` | `RoleOperator` (http.go:192) | none beyond project-scope read (`authorizeResolutionTarget`); `rejected_by` is **trusted from the request body**, not server-derived (http_escalations.go:432-478) |
| close-with-result | `POST /v1/escalations/{id}/result` | `RoleOperator` (http.go:193) | none beyond project-scope read; `closed_by` = `req.Executor`, **trusted from the request body** (http_escalations.go:503-561) |

`RoleOperator` (kbeads/internal/server/interceptors.go:25) is the general
write role — it is **not** OIDC-human-only; a comment at
http_escalations.go:132-133 spells out that "`requireRole(RoleOperator)`
short-circuits for non-OIDC callers", i.e. any bearer token with write
capability clears this gate. **Create** (any RoleOperator token, so
effectively any agent) and **approve** (RoleOperator + hard OIDC-human check,
four-eyes) are asymmetric by design — approve is the privileged-action path
the replay executor fires from (§2), so it alone enforces "a human said yes."
**Reject and close-with-result have no code-level requirement that the caller
be human** — only convention. In the observed baseline (§5) every
`github_pr_merge` escalation was closed with `operated_by =
oidc:matthewbaker1110@gmail.com`, i.e. run from the coordinator's own
human-OIDC session — but nothing in `handleCaptureEscalationResult` enforces
that; an agent holding a plain fleet/RoleOperator token could call
`gb escalation close` on its own escalation today. This is the concrete gap
epic gd-3UQKfj0nA8 exploits deliberately (crew self-merges after a reviewer
verdict) and the reason the epic's design calls for a **reviewer bead
distinct from the requester bead, both stamped on the escalation** as the
four-eyes substitute for close, since close itself carries none.

**Four-eyes** is enforced only on **approve**
(`escalationFourEyesViolation`, http_escalations.go:159), not on
close-with-result.

**Successor** (`gb escalation successor set|resolve`,
gardenio/controller/cmd/gb/successor.go:128-170) is a shared decision/escalation
mechanism: a fallback owner an agent can name so an idle/despawned requester's
open escalation gets re-routed instead of stalling forever
(successor.go:3-47).

**Reflag/pingback** (gardenio/controller/cmd/gb/escalation_pingback.go,
`gb escalation reflag`) bump priority / re-post to Slack for a stalled
escalation; both are plain field-update/notify verbs, not resolution verbs.

**The merge-capability guard** (ka-0000004913,
gardenio/controller/cmd/gb/escalation_merge_guard.go) refuses filing a merge
escalation for an MR the requester could merge itself. **It only fires for
`operation_type == "gitlab_mr_merge"`**
(gardenio/controller/cmd/gb/escalation_create.go:131) — a GitLab-era guard.
**It does not gate `github_pr_merge`**, the operation_type actually in use on
this GitHub repo today (100% of the baseline in §5). There is currently no
"can this agent merge this PR itself" check before a `github_pr_merge`
escalation is filed.

## 2. Replay executor

**Trigger**: `POST /v1/escalations/{id}/approve` → `handleApproveEscalation`
(http_escalations.go:108) records the OIDC-derived approval, transitions the
bead to `in_progress`, then calls `s.replayEscalation(ctx, bead, approvedBy)`
(http_escalations.go:204). "Denied operation" = the `operation_spec` JSON the
escalation was filed with — a beads write call (create/update/close) the
*original* caller was authz-denied from making directly. Replay re-issues
that exact call under a system machine-operator context
(`WithMachineOperator`, http_escalations.go:249) that bypasses the write
policy that produced the original denial.

**Executor registry** — `replayEscalation` (http_escalations.go:224-375) is a
hardcoded `switch fields.OperationType` with exactly three cases:

- `"create"` (line 257) → `s.createBead(...)`, with agent-spawn provenance stamping
- `"update"` (line 282) → `s.updateBead(...)`
- `"close"` (line 303) → `s.store.CloseBead(...)`
- `default` (line 322-325) → `slog.Warn("unknown operation type for escalation replay", ...)`, **returns `nil`: no side effect, no result recorded**

**`github_pr_merge` is not a case in this switch → it falls to `default` →
advisory-only, no executor.** Approving/closing a `github_pr_merge`
escalation never merges anything on GitHub. The bead only records
`result_status`/`result` via the separate close-with-result path
(`handleCaptureEscalationResult`), which — per its own doc comment
(http_escalations.go:602-607) — "never replays the denied `operation_spec`."
The actual `gh pr merge` is run by hand by whoever closes the escalation
(today: the coordinator, via the human-OIDC laptop session). This matches
every row in the §5 baseline: `result` is human-authored prose ("PR #16
merged, sha …"), never a merge produced by the daemon itself.

This three-case registry (`create`/`update`/`close`) is the **entire**
privileged-op executor surface today. No other operation_type observed in the
live queue — `k8s_apply`, `k8s_helm_deploy`, `credential_rotation`,
`gitlab_mr_merge`, `terraform_route53_dns_record`, `aws_ecr`,
`ziti_identity_management`, etc. — has an executor; every one of them is
advisory-only, resolved by a human running the real action out-of-band and
then calling `gb escalation close --result-status ...` to record it.

**Auto-close cascade gating**: an escalation wrapper task (one that carries
label `close-on-escalation-execute` and a `blocks`→escalation edge, set by
`gb escalation create --requesting-task`, gardenio/controller/cmd/gb/escalation_create.go:274-296)
is only auto-closed when the *close-with-result* path records
`result_status == "success"` (http_escalations.go:573-580,
kbeads/internal/server/auto_close.go:104 `closeOnEscalationLabel`). A
`failure`/`partial` result leaves the wrapper open on purpose ("the
operator-privileged action did not land"). `approve` (auto-replay success)
also fires the same cascade unconditionally on success
(http_escalations.go:362-371). Rejection never cascades
(http_escalations.go:576-577).

## 3. `type:role` model override, end to end

**Role bead shape** — `RoleInfo`
(gardenio/controller/internal/beadsapi/roles.go:116-247) is parsed from a
`type=role` config bead's `fields` map by `ListRoleBeads` (roles.go:252-370).
Alongside `Model` it carries `Image`, `CPURequest/Limit`,
`MemoryRequest/Limit`, `NeedsK8sAPI`, `Privileged`-adjacent toggles
(`RuntimeClassName`, `DockerEnabled`), `Account`, `AccountWhitelist`,
`ExecutionTarget`, `SessionResume`, `Ultracode`, secrets/env overrides, and
per-role skill/agent allow/deny lists. Role lookup is by **bead title as a
free-form name** (roles.go:266 `name := b.Title`) — not a fixed enum; an
agent bead's `role` field just has to match a `type:role` bead's title
(comment at roles.go:388-391: "roles are kept as free strings — an unknown
role is [allowed]"). A collision guard (`roleCandidateBeats`, roles.go:342-364)
picks a deterministic winner if two role beads share a title, preferring the
one that actually carries model/account config.

**`Model` field** (roles.go:191-198): "Model overrides the model for agents
spawned with this role, independent of which account they use... one account
(one OAuth cred, one LiteLLM key allowing N models) can back fable/opus/
sonnet roles by setting model on the role bead — no account cloning (epic
ka-0000005488)." Parsed straight from `fields["model"]` (roles.go:293).

**Spawn-time resolution** — `resolveModelOverride(cfg, project, role)`
(gardenio/controller/cmd/controller/podspec.go:2230-2248) checks the role
cache first (split on comma for multi-role agents), then falls back to the
project's model default; empty string means "inherit the account's default
model." `applyModelOverride` (podspec.go:2263-2289) is invoked from
`applyCommonConfig` immediately after `applyAccountConfig`, i.e. **role
beats account-default, and the agent-level metadata override (highest
precedence) is applied afterward by the spec builders** — precedence chain
is `global < project < role < agent` (roles.go:115).

**Pod env** — when a role/project model is set, `applyModelOverride` bakes
**both** `ANTHROPIC_MODEL` and `CLAUDE_MODEL` into the pod spec
(podspec.go:2271-2272), and also re-tags the LiteLLM cost-attribution tag
(`retagLiteLLMModel`) and sets `BOAT_MODEL_PINNED` (`EnvModelPinned`,
podspec.go:2289) so a later coopmux credential rotation can't silently revert
the override — the in-pod `gb init` loop re-asserts the pin after any
credential reload (gardenio/controller/cmd/gb/init_agent_loop.go:791-809).

**In-pod → LLM call** — Claude Code reads `CLAUDE_MODEL`/`ANTHROPIC_MODEL`
directly; `gb`'s agent-loop command builder appends `--model $CLAUDE_MODEL`
to the Claude Code invocation when set (init_agent_loop.go:834-835). Claude
Code sends that model name as the model parameter on every request, which
hits the fleet's LiteLLM proxy (`api_base` per §3 below), which routes by
`model_name` to the configured upstream. Once baked at pod-spawn this is
**immutable for the pod's lifetime** — the Slack bot explicitly refuses a
live `--model` switch mid-session ("`ANTHROPIC_MODEL` is baked into the pod
when it spawns", gardenio/controller/internal/bridge/bot_mentions.go:1101-1116); changing
model means killing and respawning the agent.

**Models LiteLLM actually routes today** (default model list, no
`.Values.litellm.models` override — gardenio/helm/gardenio/templates/litellm/configmap.yaml:11-104),
all OAuth-forwarding to `https://api.anthropic.com`:

| `model_name` (what a role/account pins) | Upstream `litellm_params.model` |
|---|---|
| `claude-opus-5-oauth` | `anthropic/claude-opus-5` |
| `claude-opus-4-8-oauth` | `anthropic/claude-opus-4-8` |
| `claude-opus-4-7-oauth` | `anthropic/claude-opus-4-7` |
| `claude-opus-4-6-oauth` | `anthropic/claude-opus-4-6` |
| `claude-sonnet-4-6-oauth` | `anthropic/claude-sonnet-4-6` |
| `claude-sonnet-5-oauth` | `anthropic/claude-sonnet-5` (also the bare `sonnet` alias Claude Code >= 2.1.257 resolves to, configmap.yaml:52-58) |
| `claude-fable-5-1-oauth` | `anthropic/claude-fable-5-1` (current high-intelligence/`fable` tier, configmap.yaml:66-70) |
| `claude-fable-5-oauth` | `anthropic/claude-fable-5` (superseded by fable-5-1, kept for rollback) |
| `anthropic/claude-haiku-4-5` | `anthropic/claude-haiku-4-5-20251001` |
| `anthropic/claude-haiku-4-5-20251001` | same (dated pin) |

For "Sonnet asks, Opus reviews" (the epic's design), the relevant pair today
is `claude-sonnet-5-oauth` (requester tier) vs. `claude-opus-5-oauth` (a
plausible reviewer tier) — both already served, no chart change needed to
stand up a `reviewer` role pinned to Opus 5.

**Per-1M-token pricing**: **not stored in this repo.** Grepped
`gardenio/helm`, `gardenio/controller`, `deploy-values` for
`input_cost_per_token`/`output_cost_per_token`/`model_cost`/`price` — none of
the `model_list` entries above set a cost override, so LiteLLM falls back to
its own built-in default price table keyed by the `litellm_params.model`
string (`anthropic/claude-...`), which this repo does not vendor or mirror.
Actual spend is **recomputed after the fact from LiteLLM's own accounting**,
not from a local price table: `gardenio/controller/internal/spendlogs/spendlogs.go`
pages `GET /spend/logs/v2` and sums the `spend` field LiteLLM itself computed
per request (spendlogs.go:1-35, 127). Budget bead gd-51qNLIeaLO ($250 stated
vs $200 actual ceiling) is reconciled the same way — against LiteLLM's
reported spend, not a repo-local per-model $/1M table. **No concrete $/1M
number for opus-5 vs sonnet-5 could be sourced from the codebase**; that
number lives only in LiteLLM's runtime price table / Anthropic's own pricing,
neither of which is checked into this repo.

**Does a `reviewer` role bead exist yet?** No. Live query
(`kd list --type role`) returns exactly two role beads today:
`gate-probe-delete-me` (closed) and `k8s-live` (open, no model override).
No `reviewer`-named or model-pinned role bead exists — the epic's "one bead,
not a code change" is accurate but **not yet done**.

## 4. How a crew blocks on an escalation today

**Correction to the task framing: it is not `gb yield`.** `gb yield`
(gardenio/controller/cmd/gb/yield.go:20-41) blocks *only* on `type=decision`
beads — it lists open decisions filtered by
`fields["requesting_agent_bead_id"] == agentID` (yield.go:61-82) and has zero
references to escalations anywhere in yield.go/yield_freeze.go. Its own
`--help` text is explicit that `gb yield` is for a live-parked pod waiting on
"the operator's answer to finish the CURRENT turn," and that filing a
decision/escalation with no further work this turn should instead end with
`gb stop` (yield.go:34-41).

**The actual escalation-blocking mechanism** is async, not a live wait:

1. `gb escalation create --requesting-task <task>` writes a **forward
   `blocks` dependency edge** from the requester's task bead to the new
   escalation bead (escalation_create.go:266-282: "the requesting task gets a
   forward `blocks` dep on the escalation — 'task is blocked by
   escalation'"). This is what `/plans` and `gb ready`'s blocked-work filter
   read; it is not a live socket/poll.
2. If the caller also wants the daemon to auto-close the wrapper task once
   the escalation resolves successfully, it stamps the requesting task with
   label `close-on-escalation-execute` (escalation_create.go:284,
   `closeOnEscalationLabel` contract, kbeads/internal/server/auto_close.go:104).
3. The crew then calls **`gb stop`**, ending its pod/turn. Nothing is parked
   live.
4. **Resume**: when the escalation closes with `result_status == "success"`
   (either via auto-replay on `approve`, or via `gb escalation close
   --result-status success` for an advisory op like `github_pr_merge`), the
   daemon's `afterBeadClosed` → `maybeCloseEscalationWrappers` cascade
   auto-closes the wrapper task (auto_close.go, http_escalations.go:573-580).
   A closed/auto-closed task bead is what re-triggers dispatch of a (possibly
   new) agent pod to pick up the follow-on work — the same respawn path `gb
   stop`'s own docs describe for decisions ("a decision filed with
   `--requesting-task` persists as the durable re-entry handle and
   AUTO-RESPAWNS you the moment the operator resolves it", yield.go:37-39).
   `failure`/`partial` results leave the wrapper open — no auto-resume; a
   human or a follow-up agent has to act.

So: `gb yield` is the *decision*-blocking primitive; escalations use a
durable `blocks`-edge + opt-in label + `gb stop`/auto-close-cascade pattern
instead, precisely because (per yield.go's own comment) escalation waits are
often long/multi-day and a live-parked pod would just get torn down by
cooldown.

## 5. Baseline: every `github_pr_merge` escalation, last 3 days

Queried `gb escalation list --status <all>` (43 total escalations returned,
17 `github_pr_merge`), all within the last 3 days (earliest 2026-09-19
17:16 UTC, current time 2026-09-20 ~20:20 UTC). Every single one was resolved
(`operated_by`) from the coordinator's human-OIDC identity
(`oidc:matthewbaker1110@gmail.com`) — confirming the epic's claim that "the
only consumer is the coordinator laptop session."

| Created (UTC) | ID | PR / subject | Time to close (min) | Verdict | Filed by |
|---|---|---|---|---|---|
| 09-19 17:16 | gd-Nd5VzLS1Ax | type:role privilege gate PR + deploy | 241.6 | success | gardenio-add-privileged-to-ebe |
| 09-20 01:50 | gd-7Xa9Xtn9V3 | PR #9 ziti-ctrl-backup offline-guard fix | 763.2 | success | gardenio-plan-gardener-for-gd-vn8uwtbq9s-uaxg |
| 09-20 03:09 | gd-OLmPe3T8rv | PR #12 taint registry schema migration 047 | 40.1 | success | gardenio-add-taint-registry-i76 |
| 09-20 04:46 | gd-Vguu8CnVLj | PR #14 type:team config bead + TeamsOf | 6.9 | success | gardenio-typeteam-config-bead-a1b |
| 09-20 05:53 | gd-twN4r92jvj | PR #16 SetTaint/GetTaint/ListTaints store methods | 11.8 | success | gardenio-gardener-for-gd-6fmhiieilr-wlca |
| 09-20 06:50 | gd-4vLjyotJeH | PR #18 kd team CLI | 3.4 | success | gardenio-kd-team-createaddremoveshowlist-tmt |
| 09-20 07:35 | gd-DDc9JdUTft | PR #21 release.sh repo-root-safe path fix | 464.6 | success | gardenio-audit-releasesh-against-ise |
| 09-20 10:33 | gd-pMfYd1eaTv | PR #23 taint_grants store/API grant/revoke/apply | 198.6 | success | gardenio-taintgrants-storeapi-grant-g7t |
| 09-20 10:33 | gd-Vd3hVMICQe | PR #19 controller-base shared layer | 213.5 | success | gardenio-prototype-shared-base-61g |
| 09-20 11:41 | gd-jaUXsWYoWu | PR #24 taint create/list/show endpoints/CLI | 11.5 | **failure** (sent back) | gardenio-taint-createlistshow-endpoints-01b |
| 09-20 12:52 | gd-2bfhlArNeL | PR #24 re-escalation after fixes | 147.5 | success | gardenio-gardener-for-gd-6fmhiieilr-b2bn |
| 09-20 13:06 | gd-yIITgxe6lC | PR #26 quench prebuilt-binary fix | 132.9 | success | gardenio-gardener-for-gd-irs5c9bebi-ci-bui-86f0 |
| 09-20 15:07 | gd-lGzCQ9vZ6w | PR #28 quench docs/escapes fix | 12.3 | success | gardenio-gardener-for-gd-irs5c9bebi-ci-bui-vukz |
| 09-20 16:18 | gd-FBgJFhneSK | PR #32 remove dead Makefile lint references | 121.9 | success | gardenio-plan-gardener-for-gd-ljovmxrw8o-sg4s |
| 09-20 16:22 | gd-GRWGLtRLFG | PR #34 primary static token taint gate | — | **open** (not yet resolved) | gardenio-apply-the-taint-e18 |
| 09-20 18:52 | gd-6DBiE3do64 | ADR fix: detaint forks a successor | 3.4 | **partial** (needs human) | gardenio-plan-gardener-gd-6fmhiieilr-e835 |
| 09-20 20:12 | gd-TXeNzwbLZ0 | ADR fix: detaint fork\|strip, policy-selectable | 1.5 | success | gardenio-plan-gardener-gd-6fmhiieilr-e835 |

**Summary** (16 closed of 17; 1 still open at time of writing):

- Verdict split: **14 success (mergeable)**, **1 failure (sent back)**, **1
  partial (needs human)**, 1 still open.
- Mean time-to-close (16 closed rows): **~148 min (2.5h)**.
- Median: ~40 min. Spread is wide (1.5 min to 12.7h/763 min) — the
  coordinator is a single human session working other things between
  escalation sweeps, not a dedicated always-on reviewer; this variance is
  exactly the bottleneck the epic's agent-review loop targets.
- Zero rows show the daemon itself recording a merge SHA or executing
  anything — consistent with §2's finding that `github_pr_merge` has no
  replay executor; every `result` is coordinator-authored prose confirming a
  manual `gh pr merge` (or, for `failure`/`partial`, requested changes /
  a handoff note).

This is Phase 3's baseline for comparison: once an `agent-review` reviewer
role exists, its close-to-open latency and verdict distribution should be
measured against this table (~2.5h mean / ~40min median, 82% success /
6% sent-back / 6% needs-human / 6% still-open-at-snapshot on this window).
