# E2E coverage gap: operated_by read path on comments/events activity feed

Bead chain: ka-0000005811 (cycle c coverage-gap pass) → ka-0000007045
(impl follow-up). Source MR:

- !2040 — `fix(operated_by): read on comments/events; resolve
  slack→email at write` (`b71ae811`).

## What changed

Before !2040 the postgres store's `queryGetComments` /
`queryGetEvents` did not include the `operated_by` column in their
`SELECT`, and `scanComment` / `scanEvent` had no destination field
for it. As a result `Comment.OperatedBy` and `Event.OperatedBy` were
**always** empty when read out of postgres, even though writes had
been stamping the column for weeks.

`gardenio/controller/cmd/beads-viewer/bead_activity.go::activityActor`
implements a documented precedence:

```
operated_by → actor → payload.bead.assignee → payload.bead.created_by
```

Because the read-path returned `OperatedBy=""`, the per-bead activity
feed (LV-1, rendered on `/bead/{id}`) effectively skipped step 1 and
fell straight through to `actor` for every comment/event. !2040 wires
the column back through, restoring the documented precedence.

The same MR also resolves Slack `Uxxx` operator IDs to email at write
time in five bridge surfaces (`bot.go`, `bot_commands.go`,
`bot_mentions.go`, `bot_reactions.go`, `bot_human_survey_rating.go`),
so freshly-written rows now carry a stable `oidc:<email>` operator
id rather than a Slack ID that can drift across workspaces.

## Coverage today

| Layer | Surface | Coverage |
| --- | --- | --- |
| SQL store | `queryGetComments` / `queryGetEvents` `SELECT operated_by` and scan into model field | `kbeads/internal/store/postgres/postgres_test.go` updated in !2040 — both populated and NULL row asserted |
| Bridge write | slack→email resolution on rating responses | `bot_human_survey_rating_test.go` updated in !2040 |
| Bridge write | slack→email resolution on `bot.go`, `bot_mentions.go`, `bot_reactions.go`, `bot_commands.go` | **No new unit test** — these call into the same `resolveOperatedBy` helper but their happy/error paths around it are not asserted |
| Beads server audit | `applyOperatedByHeader`, `resolveOperatedBy` (already present pre-!2040) | covered by `kbeads/internal/server/audit_test.go` |
| Viewer activity feed | `activityActor` precedence on `Event.OperatedBy` | `bead_activity_test.go` (added with LV-1) — table-driven cases including `operated_by-wins` and `actor-when-no-operated_by` |
| **End-to-end** | bridge stamps operated_by → store persists → viewer renders chip on `/bead/{id}` | **None** |

## Gap

There is no e2e module that exercises the integration path:

1. A comment/event is written carrying a known `operated_by` value
   (via either the bridge or a direct `X-Operated-By` HTTP request
   that mirrors what `kd` sends).
2. The kbeads server persists the column (validated indirectly by
   audit + sql unit tests).
3. The viewer's `/bead/{id}` HTML, when GET-ed by an authenticated
   user, contains an operator chip whose value matches what was
   stamped — i.e. `activityActor` returned the operated_by value, not
   the actor or a payload field.

This is the integration shape that !2040 unblocks. None of the layer
unit tests would catch a regression in any of the cross-layer
contracts: a JSON field rename in `client_comments.go`, a missing
template helper invocation, or a server-side header that gets dropped
between gRPC and HTTP transports.

## Why this isn't a one-cycle e2e write

Hitting `/bead/{id}` against a deployed viewer pod requires a real
Keycloak-issued bearer token — same architectural prerequisite as
`test-viewer-migration-banner.sh`. That helper now exists
(`gardenio/tests/e2e/scripts/lib-gitlab-token.sh::mint_user_token` — the
`mint_user_token` API introduced for ka-0000000489, preserved when the
password-grant `lib-keycloak-token.sh` was retired in T6b, ka-0000001463), so
the prerequisite is met for the *token* side.

What is **not** yet shared between viewer e2e modules:

- `setup_viewer_portforward` / `setup_beads_portforward` /
  `viewer_get` / `viewer_post` are inlined in
  `test-viewer-migration-banner.sh`. A second viewer-touching module
  should extract them into a `lib-viewer.sh` shim rather than
  duplicate them, otherwise the run-shard suite ends up with two
  divergent copies of the curl/port-forward boilerplate.

The impl task (ka-0000007045) should land that extraction first or
inline it in the new module with a TODO to extract on the third
caller.

## Recommended scenarios

For `gardenio/tests/e2e/scripts/test-viewer-bead-activity.sh`,
wired into `run-shard.sh` under module name `viewer-bead-activity`:

1. **Comment with explicit operator renders chip on /bead/{id}.**
   - Mint a non-Matt token: `mint_user_token e2e-not-matt-$$@example.com`.
   - Create a test bead via `kd create` (idempotent title prefix
     `viewer-bead-activity-e2e`).
   - Add a comment carrying an explicit `oidc:e2e-not-matt-$$@example.com`
     operator id — either via `kd comment add <id> --body ... --operated-by oidc:…`
     (CLI path) or direct HTTP `POST /v1/beads/{id}/comments` with
     header `X-Operated-By: oidc:…`.
   - GET `/bead/{id}` from the viewer via the minted token.
   - Assert response 200 and HTML contains an operator chip whose
     `data-operator` (or visible text) matches the stamped operator.
   - Cleanup: `kd close <id>`.

2. **Event with operator renders in activity feed actor column.**
   - Same bead, transition state by claiming/closing under a known
     operated_by (e.g. `BEADS_OPERATED_BY=oidc:claimer@example.com kd
     claim <id>`).
   - GET `/bead/{id}`; locate the row whose icon corresponds to
     `beads.bead.updated` / `beads.assignee.changed` (whichever
     `claim` emits — confirm against `bead_activity.go::topicIcon`).
   - Assert that row's actor cell renders `claimer@example.com` (the
     operator), NOT the `KD_ACTOR` writing the request.
   - Reasoning: this exercises the `operated_by → actor` precedence
     end-to-end. If !2040 regressed, the cell would show `KD_ACTOR`
     because `operated_by` would be empty after the SELECT.

3. **Negative: scheduler-stamped event renders sentinel chip.**
   - File a `type=schedule` bead OR pick an existing scheduler bead
     and locate one of its emitted events on a child task. Either:
     (a) seed by calling the http API with `X-Operated-By:` omitted
     entirely, simulating an autonomous write, OR
     (b) use a captured fixture under `claudeless/`.
   - GET `/bead/{id}`; assert the activity row uses the
     scheduler-sentinel branch in `operator_chip.go::resolveOperatorChip`
     (the `Kind="scheduler"` rendering when `OperatedBy` is empty AND
     `CreatedBy` matches the scheduler pattern), NOT the empty-string
     fallthrough.
   - This guards against a regression where !2040's scan defaults a
     missing operator to a stray `"null"` literal or similar.

## Lower-cost intermediate step

While the impl task is pending, the cheapest backstop is a
`bridge.bot` slack→email resolution test for **all five** rewritten
write surfaces, not just `bot_human_survey_rating.go`. Today only
the rating path has a regression test for the new resolution logic.
A small unit test alongside the existing bot_*_test.go files would
lock in the contract without any e2e plumbing.

The impl task captures both — the e2e module is the primary
deliverable, the unit-test parity is a secondary backstop the same
crew can land in the same MR.

## Plan-of-record

- This doc + the follow-up impl task (ka-0000007045) capture the
  scope. The impl task is filed at `priority=2` since the layer-unit
  coverage already prevents the most obvious regressions; the
  end-to-end integration shape is what's missing.
- No new e2e module is added in this MR.
- The new module should reuse `lib-gitlab-token.sh::mint_user_token`
  and either reuse or extract `setup_viewer_portforward` /
  `viewer_get` from `test-viewer-migration-banner.sh` into
  `lib-viewer.sh`.
- Idempotency contract: the new module must close all beads it
  creates in cleanup, and key its created-bead titles on a prefix
  that other modules (and re-runs) will not collide with —
  `viewer-bead-activity-e2e-<random>`.
