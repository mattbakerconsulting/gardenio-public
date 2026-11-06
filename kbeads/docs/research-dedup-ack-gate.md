# Research: Decision/Escalation Dedup-Ack Gate

**Status**: Research / plan-of-record — T1 (server) + T2 (kd CLI) + T3 (tests) landed 2026-08-13, gate behind `BEADS_DEDUP_GATE_ENABLED` (default OFF)
**Author**: gardenio-r1-design-research-vlp
**Date**: 2026-05-25
**Epic**: ka-0000004599
**Task**: ka-0000000073
**Origin**: MR !2982 — 2 stale open decisions about the same MR before a 3rd gardener firing self-merged it. Systemic gardener duplicate-decision pattern; supersedes ka-0000001073 (prompt-side workaround).

## Implementation status (2026-08-13, ka-0000005822)

The gate ships **dark**: with `BEADS_DEDUP_GATE_ENABLED` unset/false the
daemon's create path is byte-for-byte unchanged. What landed:

- **T1 server** (`kbeads/internal/server/dedup_ack.go`): `applyDedupAckGate`
  wired into `createBead`'s `switch` for `decision` and `escalation` (dedup
  check runs *before* the pre-existing ack-only decision guard, which is
  untouched). Hash = `sha256(sorted "<id>\t<title>\n")` over the full open
  list. HTTP → 409 `{code:"dedup_ack_required", open:[…], hash, retry_with}`;
  gRPC → `FailedPrecondition` with a JSON payload embedded in the message
  (mirrors `grpcOperatorRefusal`). Env knobs: `BEADS_DEDUP_GATE_ENABLED`
  (default off) and `BEADS_DEDUP_GATE_PROJECTS` (optional CSV allowlist).
- **T2 kd CLI** (`kbeads/cmd/kd/create.go`, `internal/client`): flags
  `--saw-open-decisions=<hash>` / `--saw-open-escalations=<hash>` and a typed
  `DedupAckRequiredError` that renders the open list + retry command to stderr.
  `--force` now also bypasses the gate (plumbed as `CreateBeadRequest.Force`).
- **T3 tests**: `dedup_ack_test.go` (server, 13 scenarios incl. race, cap,
  cross-type, replay/force bypass, no-project, allowlist, flag-off) +
  `internal/client/dedup_ack_test.go` (409 decode).

**Deviation from the design**: the ack token rides the dedicated top-level
input field `saw_open_hash` (CLI flag / HTTP body / query param), **not** a
`fields.saw_open_<type>` entry. The strict decision/escalation type schemas
reject unknown *bead fields* in `ValidateTypeFields` (which runs before the
gate), so a `fields.saw_open_*` value would 400 as "unknown field" and never
reach the gate. A top-level input field is schema-exempt.

**Deferred to follow-ups** (not in this MR): T4 gardener-prompt paragraph;
adding a proto field for the gRPC round-trip of the ack token (needs protoc v6
regen — the kd CLI defaults to HTTP, so the feature is fully usable today);
threading `SawOpenHash` through the gardenio controller's `beadsapi` client so
`gb decision/escalation create` can auto-retry (today they can `--force`).
The gate must not be flipped ON in prod until T4 lands and the legacy
duplicate backlog is swept (see the closing "Out of scope" note).

## TL;DR

When a caller (agent or human) creates a `decision` or `escalation` bead, the
daemon will reject the create with **HTTP 409 Conflict / gRPC FailedPrecondition**
if one or more open beads of the same type already exist in the same project,
unless the caller passes a flag that echoes a server-computed hash of the open
list. Same pattern as the existing operator-refusal gate (`code:
operator_refusal`, see ka-0000006731), one round-trip wider.

The 14 open questions in the parent task collapse into:

1. **Hash content**: `sha256(sorted "<id>\t<title>\n" lines)` — title-stability is sufficient; status churn is what we *want* the caller to look at, not what should force a re-ack.
2. **Hash leakage**: opaque sha256 in the round-trip, readable list in the rejection body.
3. **Flag name**: **`--saw-open-decisions=<hash>`** / **`--saw-open-escalations=<hash>`** (type-specific, terse, reads naturally in gardener prose).
4. **Status code**: **HTTP 409 Conflict**, `code: dedup_ack_required` (separates from `operator_refusal` 400 which is a different class of failure).
5. **Race policy**: hard reject on hash mismatch — additive-tolerance is operator-friendlier in theory and a footgun in practice (see §5).
6. **Cross-type snapshot**: separate queues — decisions and escalations are different operator surfaces.
7. **No-project fallback**: skip the gate when no `project:<name>` label resolves; do not synthesize a `global` bucket.
8. **List cap**: cap at 100 most-recent, `truncated: true` flag, `total_open: <n>` for honesty.
9. **Escalation replay**: replay path sets `Force=true`, audit-logged at WARN with `reason=escalation_replay`.
10. **OIDC humans**: same gate, no exemption. The kd CLI prompt-mode flow handles the friction.
11. **Audit logging**: INFO on rejection (measurable gate effectiveness); WARN on `Force=true` bypass.
12. **Rollout**: `BEADS_DEDUP_GATE_ENABLED` env var, default **false**. Flip true after T4 (gardener prompt updates) lands. Per-project allowlist (`BEADS_DEDUP_GATE_PROJECTS=gardenio,...`) for staged rollout.
13. **Gardener prompts**: one-paragraph addition required for every gardener template. CLI auto-prompts only help in interactive mode; gardeners are non-interactive.
14. **Tests**: 8 acceptance scenarios listed in §14.

Implementation work resumes under tasks T1 (server), T2 (CLI), T3 (tests), T4 (gardener prompts) of epic ka-0000004599 after operator sign-off.

## Table of contents

- [1. Problem statement](#1-problem-statement)
- [2. Hash content (Q1)](#2-hash-content-q1)
- [3. Hash determinism vs leakage (Q2)](#3-hash-determinism-vs-leakage-q2)
- [4. Flag name (Q3)](#4-flag-name-q3)
- [5. Error code and HTTP status (Q4)](#5-error-code-and-http-status-q4)
- [6. Race policy (Q5)](#6-race-policy-q5)
- [7. Cross-type snapshot (Q6)](#7-cross-type-snapshot-q6)
- [8. No-project fallback (Q7)](#8-no-project-fallback-q7)
- [9. List size cap and display (Q8)](#9-list-size-cap-and-display-q8)
- [10. Escalation replay interaction (Q9)](#10-escalation-replay-interaction-q9)
- [11. OIDC human exemption (Q10)](#11-oidc-human-exemption-q10)
- [12. Audit logging (Q11)](#12-audit-logging-q11)
- [13. Rollout strategy (Q12)](#13-rollout-strategy-q12)
- [14. Gardener prompt changes (Q13)](#14-gardener-prompt-changes-q13)
- [15. Tests (Q14)](#15-tests-q14)
- [16. Wire format (concrete)](#16-wire-format-concrete)
- [17. Acceptance criteria summary](#17-acceptance-criteria-summary)

## 1. Problem statement

Gardeners (scheduled or trigger-spawned agents) repeatedly produce duplicate
`decision` and `escalation` beads about the same subject because they have no
durable view across firings. The canonical recent failure: MR !2982 collected
**two open decisions** asking "should we merge?" before a third gardener firing,
also unaware of the prior two, **self-merged it** — overriding both pending
operator checkpoints.

The systemic root cause is not "bad prompts." A gardener prompt that says
"check for existing decisions first" is a soft guarantee that any sufficiently
busy or context-pressured firing will violate. The systemic fix is to make
the daemon refuse the duplicate at the create choke point, the same way we
already refuse decisions with no real fork (`decision_validation.go`,
ka-0000002237) and writes lacking attribution (`operator_refusal`,
ka-0000006731).

**Design constraint**: zero new ergonomic surface for humans (kd CLI in
interactive mode auto-prompts the operator), one extra round-trip per
decision/escalation for agents — they receive the structured rejection,
parse the hash, retry.

## 2. Hash content (Q1)

**Stance**: `sha256(sorted lines of "<id>\t<title>\n")`. Title-stability is
sufficient.

**Why title-stability is sufficient.** The point of the gate is to force the
caller to read the open list before creating a duplicate. The list is "what
the operator has been asked about and not yet answered." Identifiers (id +
title) name the conversation. The caller does not need to be re-prompted
when:

- The status changes from `open` to `in_progress` (operator started looking).
- The age changes (time passes).
- `operated_by` changes (operator re-assigned).

The caller *does* need to be re-prompted when the *membership* of the open
queue changes — a new bead appears, or one disappears. Membership is fully
captured by `(id, title)` pairs.

**Why include title and not just id.** A title-only hash would still force
re-ack on membership changes, but the readable list returned to the caller
needs titles, and recomputing a parallel hash is overhead. Including titles
in the hash also catches a rare-but-real case: a gardener renames an open
decision in flight (e.g., "Self-merge MR !2982?" → "Self-merge MR !2982? —
SUPERSEDED by manual review"); the next gardener firing should re-read.

**What we explicitly exclude:** status, age, operated_by, fields,
labels-other-than-project. These are noisy and change without changing
"what the operator has been asked." If we ever want a stricter mode
(e.g. "re-ack if status changed"), it lives behind a second hash
(`saw_open_decisions_strict`) added later — not by silently changing this
hash's content.

**Ordering.** Sort lexically by `id` ascending. IDs are nanoids so this
is stable, type-pure, and trivially testable.

**Newlines and tabs.** Use `\t` between id and title; use `\n` as line
terminator (including after the last line, like POSIX text files). This
keeps hash content unambiguous when a title contains tabs or newlines —
which we forbid in titles at the model layer anyway, but defense in depth.

## 3. Hash determinism vs leakage (Q2)

**Stance**: opaque sha256 in the over-the-wire token. Readable list in the
rejection body.

**Why opaque.** A readable CSV of IDs invites callers to construct the
token client-side from a *cached* list, defeating the gate. The whole
mechanism rests on: "you only get the token from a response that proves
you saw the current open list." sha256 makes this concrete — you cannot
mint a token without the server having minted it for you (or having
computed it independently from a fully-current list, which is the same
work as querying).

**Why a readable list anyway.** The rejection body needs to be useful to
both humans (`kd` CLI surfaces it directly) and agents (which dump it back
into context). The readable list does not weaken the hash; an agent that
wants to skip looking and just echo the hash is exactly the gardener we
*want* to skip looking and just echo the hash — they have read the list
through their LLM context window even if they did not act on it.

**Encoding.** Hex-encoded sha256 (`[a-f0-9]{64}`). Base64 saves bytes but
costs CLI ergonomics (`--saw-open-decisions=` with `+`/`/` requires
quoting). Hex is unambiguous, copy-pasteable, and self-describing.

## 4. Flag name (Q3)

**Stance**: `--saw-open-decisions=<hash>` and `--saw-open-escalations=<hash>`,
type-specific.

**Why "saw-open-".** Three qualities:

- **Action-tense honest.** The caller is asserting "I have seen the open
  list." Not "I have acked everything" (untrue), not "looked at existing
  decisions" (vague — looked at the schema? the spec?). The contract is
  visibility, not endorsement.
- **Terse enough for prompts.** Gardener prompts will mention this flag
  every fire. `--saw-open-decisions=<sha>` is 27 characters + hash — short
  enough that we don't get prompt-shortening drift toward an undocumented
  alias.
- **Reads naturally in prose.** "I saw the open decisions, here is the
  hash: …" is something an LLM emits cleanly without contortion.

**Why type-specific, not a single `--dedup-token`.** Two reasons:

1. A future cross-type policy (e.g., decisions also gate on open
   escalations about the same subject) is **not** the v1 design (see §7).
   Naming the flag generically invites that scope creep.
2. The CLI's interactive prompt-mode (ka-only) needs to render
   type-appropriate text ("You have 3 open decisions in `gardenio`, are
   they about the same thing?" not "You have 3 open dedup-things…").
   Type-specific flags make the prompt code one-to-one with the flag.

**Rejected alternatives:**

- `--looked-at-existing-decisions` — verbose, no per-firing token savings,
  reads like a recommendation comment instead of a contract.
- `--ack-open-decisions` — "ack" implies action taken (operator
  decision). The caller is not acking anyone's request; they are
  acknowledging *the queue exists*.
- `--dedup-token` — too generic, opens the door to a future flag that
  means "ack open decisions AND open escalations AND open ratings…" which
  is exactly the design pressure we are trying to avoid by keeping queues
  semantically separate.

## 5. Error code and HTTP status (Q4)

**Stance**: **HTTP 409 Conflict** with body
`{"error": "...", "code": "dedup_ack_required", "open": [...], "hash": "<sha>", "truncated": false, "total_open": <n>}`.
gRPC: **FailedPrecondition** with the same JSON payload embedded in the
status message (same convention as `grpcOperatorRefusal` in `server.go:299`).

**Why 409 not 400.** The existing `operator_refusal` gate uses 400 (`"the
request is malformed because we cannot resolve who you are"`). That is
genuinely client-input-malformed. The dedup-ack gate is conceptually
different: the request is well-formed; it conflicts with current server
state (open beads exist that the caller has not acknowledged). 409
matches the HTTP spec ("the request conflicts with the current state of
the target resource"), keeps the kd CLI's classification table simple
(distinct sentinel per gate), and aligns with the gRPC choice of
FailedPrecondition (which is the canonical mapping for "state required
to perform this operation does not hold").

**Why a distinct `code`.** Stable machine-readable codes are how the kd
CLI keys its exit codes and retry logic, not the human message string
(see `operatorRefusalErrorCode = "operator_refusal"` in `server.go:262`).
`dedup_ack_required` is unambiguous and parallel to the existing pattern.

**kd CLI exit code.** Use **75** (EX_TEMPFAIL) for the agent path —
distinct from 64 (EX_USAGE used by operator_refusal). 75 reads as
"temporary failure, retry the same logical operation," which the CLI's
auto-retry can do silently in interactive mode. Final user-visible
failures (e.g., the user said "no, don't create this") still exit 1.

## 6. Race policy (Q5)

**Stance**: **hard reject** on hash mismatch. Return the new list and the
new hash.

**Why hard reject over additive-tolerance.**

- **Operational simplicity.** Additive-tolerance ("accept if no
  pre-existing decision disappeared") requires the server to track the
  caller's prior hash → prior set, which is stateful and races against
  itself (what if the caller never sent a prior hash because this is
  their first retry?). Hard reject is stateless: hash matches current →
  proceed; otherwise reject.
- **Behavioral correctness.** "A new decision appeared" is *exactly* the
  case we want to force the caller to look again. If gardener-A is filing
  a decision about "merge MR X" and during the call gardener-B files
  "merge MR X" first, gardener-A absolutely should see that and bail —
  not silently proceed because the diff was "purely additive."
- **Retry budget is one round-trip.** The cost of "additive-tolerance is
  operator-friendlier" is paid in confusing duplicates the operator has
  to clean up. The cost of "hard reject" is one extra HTTP request, which
  happens entirely inside the CLI auto-retry loop and is invisible to
  humans.

**Retry budget cap.** The CLI does at most **2 auto-retries** on dedup
rejection before giving up and surfacing the rejection to the user/agent
to handle explicitly. This prevents pathological retry loops when a fleet
is creating decisions faster than any single caller can read them. (This
is a CLI-side concern, not a server contract; documented here so T2 ships
it.)

## 7. Cross-type snapshot (Q6)

**Stance**: separate queues. A decision-create sees open decisions only;
an escalation-create sees open escalations only.

**Why separate.**

- **Operator UX.** Decisions and escalations live in different operator
  surfaces (decision dashboard, escalation queue). The gardener creating
  a decision should be re-checking "have I already asked the operator
  this question?" — not "have I already raised a permission grant
  request that happens to be about the same MR?"
- **Hash hygiene.** A cross-type hash would force a decision re-ack
  every time an unrelated escalation appears, generating churn that
  decouples the gate from its purpose.
- **Future option.** If we discover real-world cases where a gardener
  files a decision *and* an escalation about the same subject, we add
  it as a v2 cross-type signal (warn, not block) — but only after we
  have data on whether this actually happens.

## 8. No-project fallback (Q7)

**Stance**: skip the gate when no `project:<name>` label resolves on the
new bead. Do not synthesize a `project:global` bucket.

**Why skip.** Beads without project labels are cross-fleet, infra, or
admin work — typically operator-initiated, low-volume, and high-stakes.
Forcing them through a dedup gate against a global "no-project" queue
would either:

- Conflate genuinely unrelated infra decisions ("rotate vault keys"
  vs "update DNS record"), causing spurious rejections; or
- Require synthesizing per-bucket scopes (e.g. by labels[0]) that have
  no operational meaning, deepening the foot-gun.

**Why not synthesize global.** A synthetic `project:global` bucket is
worst-of-both: it pretends grouping where none exists, and it gives
agents a way to escape per-project scoping by omitting the label.

**Resolution.** Use `projectFromLabels(labels)` (`http_metrics.go:156`)
+ `projectFromFields(fields)` (`write_authz.go:256`) — the same chain
the authz layer uses for token-scope enforcement. If both return
`""`/`"unknown"`, skip the gate and audit-log at INFO with
`reason=no_project_scope` so we can measure how often this fires.

**Open corollary.** If telemetry shows >5% of decisions/escalations
land in the no-project bucket, we revisit by making project a
required label for these two types (separate epic).

## 9. List size cap and display (Q8)

**Stance**: cap at **100** most-recent (by `created_at` DESC); response
body includes `truncated: <bool>` and `total_open: <n>`. The hash is
computed over the **full** open list, **not the truncated 100**.

**Why cap the display, not the hash.** The hash's purpose is to
guarantee the caller has seen the complete current membership. Capping
the hash input at 100 would silently let creates proceed in environments
with 101+ open decisions even when membership churns. Capping the
display saves bytes; the caller still hashes against the full set.

**Why 100.** At ~100 bytes per `(id, title)` pair, 100 is ~10KB which
is reasonable to render in CLI output and to dump back into agent
context. Gardener prompts can tolerate this. A truncation flag tells
the operator the list is incomplete, with a follow-on hint to
`kd list --type=decision --status=open --project=<p>` for the full
view.

**Why `total_open` in the body.** Operators sometimes want to know
"how bad is the queue" without paging — surfaces the metric where
the gate fires. Cheap to compute (one COUNT(*) we are doing anyway).

**Operational metric.** Export
`beads_dedup_gate_open_count{type=...,project=...}` as a Prometheus
gauge so we can watch for queue inflation independently of the gate
firing rate.

## 10. Escalation replay interaction (Q9)

**Stance**: the escalation replay path sets `createBeadInput.Force=true`
(audit-logged at WARN with `reason=escalation_replay`).

**Why.** `handleEscalation` (`write_authz.go:283`) currently creates
the escalation bead via `s.createBead(ctx, createBeadInput{...})` with
no Force. **Crucially**, the *replay* — the second create that
materializes the original requested operation after the grant — also
flows through `createBead`. If the dedup gate fires during replay, the
operator has *already granted* the action; forcing them to re-ack a
queue at this point is operationally wrong: the grant decision already
happened, the queue snapshot they saw at grant time is what they
implicitly approved.

**Implementation note for T1.** Identify replay creates by a context
flag (`isEscalationReplay(ctx)`) set inside the replay handler before
the `createBead` call. Do not key off "the calling subject is an
escalation" — replay must be the only path that gets the bypass.

**Audit format.** WARN log line:
`dedup gate bypassed: reason=escalation_replay actor=<x> escalation_id=<y> created_bead_id=<z>`.
Stamp the resulting bead's audit row with `dedup_bypass=escalation_replay`
so the bypass is queryable in the audit log.

## 11. OIDC human exemption (Q10)

**Stance**: no exemption. Humans hit the same gate.

**Rationale.**

- **Fairness.** Agents and humans should be subject to the same
  duplicate-protection rules; exempting humans incentivizes "log in as
  human" as a workaround.
- **Simplicity.** A single code path is easier to test, audit, and
  reason about.
- **Friction is bounded.** The kd CLI's interactive mode (TTY detected
  + not `--no-prompt`) catches the rejection, displays the open list,
  prompts "Continue creating? [y/N]", and on `y` retries with the
  hash. Humans see one extra confirmation, not a workflow break.
- **Real-world rate.** Humans file decisions/escalations rarely
  compared to gardener firings; the marginal friction cost is small.

**Non-TTY humans.** If a human runs `kd decision create` in a script
(no TTY, no `--saw-open-decisions`), the create fails with the dedup
rejection. This is correct: the script should be updated to read the
queue and pass the flag, just as agents must.

## 12. Audit logging (Q11)

**Stance**:

- **Rejection** (gate fires, caller did not pass valid hash): INFO log.
- **Bypass** (`Force=true`, including escalation replay): WARN log.
- **Successful create with valid hash**: no extra log (already covered
  by the standard `BeadCreated` event).

**Why INFO not DEBUG for rejection.** INFO gives us a queryable signal
to measure gate effectiveness ("how many duplicate gardener firings did
this stop this week?"). DEBUG would be noise in dev environments and
silent in prod.

**Why WARN for bypass.** Force-bypass is a signal of a meaningful
override; we want to see it in default log levels. The two legitimate
sources of bypass are (a) escalation replay and (b)
operator-with-`--force`. Both warrant a WARN line.

**Fields to log.** `actor` (subject), `bead_type` (decision|escalation),
`project`, `open_count`, `provided_hash`, `expected_hash`,
`reason` (only on bypass).

**Metric.** Export
`beads_dedup_gate_outcome_total{type,project,outcome=reject|accept|bypass}`
counter. Outcome=accept is for successful matched-hash creates so we
can compute the rejection rate.

## 13. Rollout strategy (Q12)

**Stance**: feature flag `BEADS_DEDUP_GATE_ENABLED`, default `false`.
Per-project allowlist via `BEADS_DEDUP_GATE_PROJECTS=gardenio,otherproj`
for staged enable.

**Rollout sequence.**

1. **T1 (server)**: ship the gate code, flag defaults `false`. No
   behavior change anywhere. Tests run with flag forced `true` so
   the path is covered before any prod traffic exercises it.
2. **T2 (CLI)**: ship the flag-passing and structured-rejection
   parsing. CLI is backward-compatible: if it sees the rejection
   from a daemon with gate enabled, it auto-retries; if it never
   sees the rejection (flag off), the new code is dormant.
3. **T4 (gardener prompts)**: update every gardener prompt to mention
   the flag explicitly. Without this, agents won't know to send the
   flag, and we'd see 100% rejection of first-shot creates.
4. **Enable `gardenio` project only**: flip `BEADS_DEDUP_GATE_PROJECTS=gardenio`,
   `BEADS_DEDUP_GATE_ENABLED=true` in deploy-values. Watch the
   `beads_dedup_gate_outcome_total` metric for one week.
5. **Expand to all projects**: drop the allowlist, gate is universal.

**Why a feature flag at all.** "Hard cut" is tempting but risky for a
gate that touches every gardener firing across the fleet — a bug here
would block every decision/escalation creation simultaneously. The
flag is the kill switch.

**Why an allowlist on top of the boolean.** Lets us enable per project
without redeploying. Once the gate is universal, drop the allowlist
(do not keep it around — dead flags rot).

**Removal plan.** Once the gate has run universally for two weeks
without rollback, file a follow-up bead to delete both flags. We do
not keep emergency switches indefinitely (per advice: "Don't add
feature flags or backwards-compatibility shims when you can just
change the code").

## 14. Gardener prompt changes (Q13)

**Stance**: a one-paragraph addition to every gardener prompt is
**required**; CLI auto-retry alone is insufficient for non-interactive
agents.

**Why prompt edits are required.** The CLI's auto-retry only works in
interactive (TTY) mode where it can re-prompt the human. Gardeners run
in non-interactive mode — they call `kd decision create` with
arguments, the rejection comes back, and they have to know to:

1. Read the rejection body.
2. Extract the `hash` field.
3. Re-call `kd decision create` with `--saw-open-decisions=<hash>`.

Without the prompt addition, a gardener just sees an error, surfaces
it via `gb squawk`, and gives up. With the prompt addition, the
gardener follows the documented retry pattern.

**Universal prompt paragraph.** Add to every gardener prompt (delivery,
release-owner, intake, reviver, reaper-disable-revisor):

> **Filing decisions or escalations.** Before creating a new `decision`
> or `escalation` bead, run `kd list --type=decision --status=open --project=<your-project>`
> (or `--type=escalation`) and read the result. If the create is
> rejected with `code: dedup_ack_required`, the response body
> contains the list you should have read and a `hash` field; if no
> open bead duplicates your intent, retry the create with
> `--saw-open-decisions=<hash>` (or `--saw-open-escalations=<hash>`).
> If an open bead already covers your intent, **do not file a new one** —
> comment on the existing bead instead (`kd comment add <id>`).

**Where the prompts live.** Gardener schedules and trigger templates
under `gardenio/` and inside formula beads. T4 enumerates each and
patches inline. (Operator review pass after the first three gardeners
are patched will catch missed templates.)

**Why include the "comment instead" clause.** Without it, a gardener
that *does* see the existing decision will helpfully re-file it "with
more context" — exactly the failure mode the gate is designed to
prevent. The right behavior is to add to the existing thread.

## 15. Tests (Q14)

The parent task lists seven scenarios; this section formalizes them
plus an eighth, and names the test file conventions (`*_test.go` next
to the server code, table-driven where the scenarios share scaffolding).

| # | Scenario | What it asserts |
|---|---|---|
| 1 | `dedup-happy` | Caller queries open list, hashes it, passes the hash → create succeeds, BeadCreated event fires. |
| 2 | `empty-list` | Zero open beads in scope → create succeeds without any flag (gate is no-op). |
| 3 | `mismatched-hash` | Caller passes a stale hash → 409 with current list + new hash. |
| 4 | `race-additive` | New decision appears between list and create → hash mismatch → reject with refreshed list (hard reject, per §6). |
| 5 | `type-filter` | Creating a `task` (or any non-decision/non-escalation type) → gate never fires. |
| 6 | `force-bypass` | `Force=true` skips gate even with open beads present; WARN log emitted. |
| 7 | `escalation-replay` | Replay path creates the original escalation target without dedup check; audit log records `dedup_bypass=escalation_replay`. |
| 8 | `no-project-skip` | Bead with no `project:` label and no `project` field → gate skipped; audit log records `reason=no_project_scope`. |
| 9 | `cross-type-isolation` | Open escalation does **not** trigger gate on decision-create (and vice versa). |
| 10 | `truncation` | 150 open beads → response carries 100 entries, `truncated: true`, `total_open: 150`; hash matches the full 150. |
| 11 | `flag-disabled` | `BEADS_DEDUP_GATE_ENABLED=false` → gate is dormant regardless of open list. |
| 12 | `project-allowlist` | `BEADS_DEDUP_GATE_PROJECTS=foo`, creating in `project:bar` → gate is dormant for `bar`, active for `foo`. |
| 13 | `retry-budget` | CLI auto-retry limited to 2 attempts before surfacing rejection (CLI test in `cmd/kd/`). |

**Test location.** Server-side: `kbeads/internal/server/dedup_ack_test.go`,
mirroring `decision_validation_test.go` (ka-0000002237). CLI-side:
`kbeads/cmd/kd/dedup_ack_test.go`, mirroring `operator_refusal_test.go`.

**Helper.** Add `mustComputeOpenListHash(t, beads)` to the test
harness so test cases declare the open set, and the test computes
the expected hash deterministically; this prevents test breakage
from format drift (any future change to the hash content per §2
breaks all tests at once, surfacing the contract change).

## 16. Wire format (concrete)

**Rejection body** (HTTP 409, gRPC FailedPrecondition):

```json
{
  "error": "open decisions exist in project gardenio that you have not acknowledged",
  "code": "dedup_ack_required",
  "bead_type": "decision",
  "project": "gardenio",
  "hash": "9f1a2b3c…",
  "open": [
    {"id": "ka-0000004659", "title": "Self-merge MR !2982?", "age_seconds": 7320},
    {"id": "ka-0000002998", "title": "Self-merge MR !2982 (retry)", "age_seconds": 1840}
  ],
  "truncated": false,
  "total_open": 2,
  "retry_with": "--saw-open-decisions=9f1a2b3c…"
}
```

`age_seconds` is convenience-only; not part of the hash. `retry_with`
is a copy-pasteable hint, redundant with `hash`/`code` for clients
that compose their own retry.

**Hash computation** (server, Go pseudocode):

```go
func computeOpenListHash(beads []model.Bead) string {
    sort.Slice(beads, func(i, j int) bool { return beads[i].ID < beads[j].ID })
    h := sha256.New()
    for _, b := range beads {
        fmt.Fprintf(h, "%s\t%s\n", b.ID, b.Title)
    }
    return hex.EncodeToString(h.Sum(nil))
}
```

**Hook into `createBead`**: in the `switch beadType` block at
`beads.go:185`, add two cases:

```go
case model.TypeDecision, model.TypeEscalation:
    if !in.Force {
        if err := s.applyDedupAckGate(ctx, bead, in, beadType); err != nil {
            return nil, err
        }
    }
    // Decision-specific guard still runs (ack-only check is unrelated).
    if beadType == model.TypeDecision {
        if err := s.applyDecisionGuard(ctx, bead, in.Force); err != nil {
            return nil, err
        }
    }
```

The two guards are independent: ack-only-detection (ka-0000002237)
operates on a single bead's `fields.options`; dedup-ack operates on
the open queue. Ordering: dedup first (cheaper, more common
rejection), decision guard second.

## 17. Acceptance criteria summary

For operator sign-off, the eight concrete commitments are:

1. **Trigger surface**: gate fires on `createBead` for `type ∈ {decision, escalation}` when the *new bead's resolved project* has ≥1 open bead of the same type.
2. **Rejection contract**: HTTP 409 / gRPC FailedPrecondition, `code: dedup_ack_required`, body shape per §16.
3. **Hash**: `sha256(sorted "<id>\t<title>\n" lines)` over the *full* open list, hex-encoded.
4. **Flag names**: `--saw-open-decisions=<hash>` and `--saw-open-escalations=<hash>` on the kd CLI; passes through as a request field `saw_open_<type>` (snake_case in JSON, proto-encoded in gRPC).
5. **Bypass paths**: `Force=true` (CLI `--force`, escalation replay context). Always WARN-logged.
6. **Project scope**: same project as the new bead (label or fields); no-project → skip.
7. **Cross-type**: decisions and escalations are independent queues.
8. **Rollout**: `BEADS_DEDUP_GATE_ENABLED` (default `false`) + optional `BEADS_DEDUP_GATE_PROJECTS` allowlist. CLI ships forward-compatible: prints rejection cleanly even when daemon is at older version.

After sign-off, implementation tasks proceed:

- **T1** (server): handler + hash impl in `internal/server/dedup_ack.go`, wired into `createBead`. Includes structured rejection, audit logs, Prometheus metrics.
- **T2** (CLI): flag plumbing, structured-rejection parser, interactive-mode prompt, non-interactive-mode pass-through, exit code 75.
- **T3** (tests): the 13 scenarios in §15.
- **T4** (gardener prompts): the universal paragraph in §14 added to every gardener template, formula, and trigger prompt.
- **T5** (optional, deferred): consider promoting `Force=true` in escalation replay to a typed context flag rather than a boolean, so other gates added later (rating limits? quotas?) can opt into the same bypass without re-discovering this pattern.

**Out of scope for this epic** (file separate beads if desired):

- A "recently-closed" warn-tier (referenced in epic description as v2).
- Cross-type advisory ("you just filed a decision about MR X, you also have an open escalation about MR X — proceed?").
- Operator-side bulk-close UX for cleaning up the existing duplicate backlog. (Worth a one-off cleanup pass *before* the gate enables in prod, otherwise the first gardener firings will all reject against the legacy backlog.)
