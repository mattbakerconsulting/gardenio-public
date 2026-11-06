# Per-Agent Tokens — Attribution Verification Design (T5)

**Status:** Research / ADR draft (no implementation).
**Tracks:** Epic [ka-0000004236](https://beads-viewer.gardenio.e2e.example.com/ka-0000004236) — per-agent tokens.
**Bead:** [ka-0000008147](https://beads-viewer.gardenio.e2e.example.com/ka-0000008147).
**Predecessors:**
- T0 codebase state — `gardenio/docs/research/per-agent-tokens-codebase-state.md` (MR !1535).
- T1 GitLab survey — `gardenio/docs/research/per-agent-tokens-gitlab.md` (MR !1565).
- T2 GitHub survey — `gardenio/docs/research/per-agent-tokens-github.md` (ka-0000000783, MR !1570).
- T3 providers inventory — `gardenio/docs/research/per-agent-tokens-providers.md` (ka-0000007455, MR !1571).
- T4 lifecycle — `gardenio/docs/research/per-agent-tokens-lifecycle.md` (MR !1572).

**Author:** gardenio-t5-peragent-tokens-mww.
**Date:** 2026-04-26.

This ADR designs the **verification matrix** that proves end-to-end that
per-agent tokens produce correct upstream attribution. It defines (a) the
inventory of attribution surfaces we must assert against, (b) the per-surface
assertion contract, (c) the fixture(s) that exercise the surfaces, (d) the
negative-path tests that protect against regressions, and (e) the
feature-flag toggling matrix that validates *both* the per-agent path and
the fall-back-static path. Output is the design only; the fixtures
themselves are T11/T12 work, gated on operator approval.

## TL;DR

**Recommendation:** Build a single **provider-parameterised attribution
fixture** (`per-agent-tokens-attribution`) that runs once per provider
(`{gitlab, github}`) per feature-flag mode (`{per_agent, static}`), spawns
a fresh disposable agent, has it perform the four canonical write actions
(open MR/PR, push a commit, comment on issue, comment on MR/PR), and
asserts the upstream API returns the *expected* identity for each
surface. The expected identity is `<bot-user>` for GitLab in per-agent
mode, `<app-name>[bot]` (or pool member) for GitHub in per-agent mode,
and the legacy shared bot in static mode. **Negative tests** assert that
revoking the per-agent token mid-spawn causes the next API call to 401,
and that re-spawning produces a *new* upstream identity (not the prior
agent's).

**Where the fixture lives (recommendation):** in **RWX e2e**, alongside
the existing `e2e.yml` smoke suite, *not* in `gardenio/controller`'s
Go-level e2e. The reason: per-agent-token attribution is a property of
*deployed system state* (controller + beads-daemon + upstream credential
issuer + a real GitLab/GitHub project to act in), not of controller code
in isolation. Controller-side unit/integration tests should keep their
mock issuers; the verification fixture is the bridge between
mocked-issuer correctness and "in production it actually attributes
right." This is surfaced as Open Decision §6.1 — operator may prefer
splitting the fixture into Go-level mocks for the assertion contract
and an RWX-level smoke for the wire path.

**Test taxonomy** (the fixture covers all four):

| Layer | Asserts | Scope | When run |
|---|---|---|---|
| Unit (Go) | Issuer client constructs correct upstream API calls; expected fields set on K8s Secret | Per-issuer module in `controller/cmd/controller/agenttoken_*` | `make test`, every push |
| Integration (Go + fake upstream) | Lifecycle state-machine transitions correctly under simulated upstream errors, rate-limits, controller restarts | Controller + httptest fake of GitLab/GitHub | `make test`, every push |
| Wire (RWX e2e, this fixture) | Real spawn → real upstream API → upstream returns expected identity | Deployed gardenio-rwx + a dedicated test GitLab project + a test GitHub repo | Per-release e2e suite, gated by feature-flag matrix |
| Negative (RWX e2e, this fixture) | Revoked tokens 401, expired tokens rotate cleanly, fail-closed mode refuses to spawn | Same | Same |

The wire layer is the load-bearing one — it is the only layer that
catches "we put the token in the right env var but the *upstream*
permission set didn't allow the action," "the GitHub App's installation
didn't include the test repo," "kubelet didn't propagate the rewritten
Secret in time," and similar production-shaped issues. Unit tests
cannot find these; the fixture must hit a real provider.

The two open decisions T5 surfaces (in addition to T1 §6, T2 §6, T3, and
T4 §8):
**(a)** Where do verification fixtures live — RWX e2e or
`gardenio/controller` Go-level e2e? (Recommended: RWX e2e for the wire
layer.)
**(b)** Which dedicated test resources do we provision — one
`gardenio-test/per-agent-tokens-fixture` GitLab project + one GitHub repo,
or reuse `gardenio-rwx`'s existing project? (Recommended: provision
dedicated; isolation prevents fixture writes from polluting real
operator dashboards.)

## 1. Constraints carried in from T0–T4

From the predecessor docs:

1. **Per-spawn upstream-actor identity differs by provider** (T1 §1.3,
   T2 §1.3). GitLab's recommended path produces a *distinct*
   `project_NNNN_bot_xxxx` user per spawn (T1 §3). GitHub's recommended
   path produces a *single* `<app-name>[bot]` for every spawn (T2 §3),
   with per-agent identity recovered via a PR-body annotation and a
   commit trailer. **The verification matrix must distinguish these**:
   GitLab assertions can match `^project_\d+_bot_[a-z0-9]+$`; GitHub
   assertions must match `^<app-name>\[bot\]$` AND `body
   contains "Created by agent: <bead-id>"`.
2. **Two separable attribution surfaces** (T0 §1.3, T1 §1.3, T2 §1.3):
   commit-author (set via git `user.name`/`user.email`, observable as
   the diff-view "Author") and API-actor (set by the bearer token,
   observable as PR/MR/comment "Created By" / "Author"). The fixture
   must assert *both* per provider — they are independent and can
   diverge (an MR opened by the App can carry commits authored by the
   pool member, or vice versa, depending on the `git push` codepath).
3. **Feature-flag granularity** (T4 §9.1): two **per-provider**
   sub-flags (`PER_AGENT_TOKENS_GITLAB`, `PER_AGENT_TOKENS_GITHUB`)
   plus the umbrella `PER_AGENT_TOKENS` (beads-daemon) and the failure-
   mode flag (`PER_AGENT_TOKENS_FAIL_MODE`). The verification matrix
   must run per (provider × per-agent-on/off) and assert the static
   path still works when the per-agent path is off. This gives a 2×2
   per provider — 4 cells per provider — plus negative cells.
4. **Init-clone is unchanged** (T4 §5.1). The fixture does not need to
   verify init-clone behavior at the script level; it only needs to
   verify that the agent's *first* `git push` and *first* `glab/gh`
   call from inside the pod use the per-agent credential. If they do,
   the init-clone wiring is correct end-to-end.
5. **No retroactive re-attribution** (T1 §5, T2 §5). The fixture does
   not need to (and cannot) assert anything about pre-existing MRs/PRs
   under the legacy shared identity — those stay attributed to the
   legacy actor. Each fixture run creates *new* MRs/PRs and asserts
   only against those.
6. **Token TTL = 24h default** (T4 §4.4 + T1 §6.3). GitHub installation
   tokens are 1h hard cap (T2 §3 step 5) with controller re-mint at
   50min. The fixture's negative path for "expired token rotates
   cleanly" needs to either (a) wait 50–60 min for a real rotation
   (slow), or (b) force-rotate via a controller-side admin endpoint
   (`gb tokens rotate <bead-id>` per T4 §7.1). Recommend (b); §3.5
   of this doc covers it.

## 2. Attribution surface inventory

The complete list of upstream surfaces the v1 epic must produce correct
attribution for, broken out per provider. Each row is one **assertable
fact** the fixture will test.

### 2.1 GitLab (recommended path: PAT with auto-bot-user, T1 §3)

| # | Surface | Where it shows | API endpoint | Field name | Per-agent expected | Static-mode expected |
|---|---|---|---|---|---|---|
| GL-1 | MR Author | MR list "Author" column; MR header avatar | `GET /api/v4/projects/:id/merge_requests/:iid` | `author.username` | matches `^project_\d+_bot_[a-z0-9]+$` | matches `^gardenio-bot$` (or whatever the legacy username is) |
| GL-2 | MR Author user-id | Internal references | same | `author.id` | per-spawn unique numeric ID | static numeric ID |
| GL-3 | Issue Author | Issue list "Author" column | `GET /api/v4/projects/:id/issues/:iid` | `author.username` | per-spawn bot | static |
| GL-4 | MR Comment author | "Comments" stream | `GET /api/v4/projects/:id/merge_requests/:iid/notes` | `notes[].author.username` | per-spawn bot for notes whose `body` was authored by the agent | static |
| GL-5 | Issue Comment author | Issue thread | `GET /api/v4/projects/:id/issues/:iid/notes` | `notes[].author.username` | per-spawn bot | static |
| GL-6 | Commit Author (git) | Diff view "Author"; `git log` | `GET /api/v4/projects/:id/repository/commits/:sha` | `author_name`, `author_email` | matches the per-agent Secret's `username`/`email` keys (T1 §3.3) | `<role>@gardenio.local` (today's synthetic) |
| GL-7 | Commit Committer (git) | Same | same | `committer_name`, `committer_email` | same as author for our use (we don't trailer-on-commit) | same as today |
| GL-8 | Commit pushed-by event | Project events feed | `GET /api/v4/projects/:id/events?action=pushed` | `author.username` of the matching event | per-spawn bot | static |
| GL-9 | MR "merge by" actor (only when fixture merges) | MR header "merged by" | `GET /api/v4/projects/:id/merge_requests/:iid` | `merge_user.username` | per-spawn bot | static |
| GL-10 | Audit-event actor (Premium-only; skip on Free) | Group/project audit events | `GET /api/v4/projects/:id/audit_events` | `details.author_name` | per-spawn bot | static |
| GL-11 | Approval actor (when fixture approves) | MR "approved by" | `GET /api/v4/projects/:id/merge_requests/:iid/approvals` | `approved_by[].user.username` | per-spawn bot | static |
| GL-12 | `last_used_at` on the issued PAT | `GET /api/v4/projects/:id/access_tokens/:tid` | `last_used_at` | non-null after first API call | n/a (no per-agent PAT exists) |

Notes:

- GL-10 is gated on Premium tier (T1 §6.1). The fixture should detect
  the tier (404 vs 200 on the audit-events endpoint) and *skip* this
  cell rather than fail when on Free. Surfaced as part of §6.3.
- GL-9 and GL-11 require the fixture to *actually merge* / *actually
  approve*. Recommend skipping merge in the fixture (we don't want to
  pollute the test repo with merged junk) and asserting the
  approval-author surface only when the fixture role has approval
  permission. Tracker the omission as `expected_skip` so the matrix
  doesn't regress silently.
- GL-1 through GL-5 are the *high-priority* cells — these are what an
  operator looking at the GitLab UI sees first. If any of these fail
  the matrix, the rollout is not safe.

### 2.2 GitHub (recommended path: GitHub App + installation token, T2 §3)

| # | Surface | Where it shows | API endpoint | Field name | Per-agent expected (App path) | Per-agent expected (PAT-pool fallback) | Static-mode expected |
|---|---|---|---|---|---|---|---|
| GH-1 | PR Author | PR list "Author" column | `GET /repos/:o/:r/pulls/:n` | `user.login` | `<app-name>[bot]` (literal, the App's slug) | leased pool member username (`acme-ag-N`) | static shared username |
| GH-2 | PR Author type | Internal | same | `user.type` | `Bot` | `User` | `User` |
| GH-3 | PR body annotation | PR body, first paragraph | same | `body` | contains substring `Created by agent: <bead-id>` | same | n/a (legacy doesn't annotate) |
| GH-4 | Issue Author | Issue list | `GET /repos/:o/:r/issues/:n` | `user.login` | `<app-name>[bot]` | pool member | static |
| GH-5 | Issue body annotation | Issue body | same | `body` | contains `Created by agent: <bead-id>` | same | n/a |
| GH-6 | PR Comment author | PR comment thread | `GET /repos/:o/:r/issues/:n/comments` | `comments[].user.login` | `<app-name>[bot]` | pool member | static |
| GH-7 | PR Review author (when fixture reviews) | PR "Review" stream | `GET /repos/:o/:r/pulls/:n/reviews` | `reviews[].user.login` | `<app-name>[bot]` | pool member | static |
| GH-8 | Commit Author (git) | Commit page "Author" | `GET /repos/:o/:r/commits/:sha` | `commit.author.name`, `commit.author.email` | matches per-agent Secret's `username`/`email` keys (T2 §3.3 — `<installation-id>+<app-name>[bot]@users.noreply.github.com`) | pool member's noreply | `<role>@gardenio.local` |
| GH-9 | Commit Committer (git) | same | same | `commit.committer.name`, `committer.email` | same as author | same | same |
| GH-10 | Commit "associated user" (GitHub UI badge) | Commit page avatar | same | `author.login` (GH user that owns the email) | the App's bot user | pool member | static |
| GH-11 | Commit message trailer | `git log --show-signature` of the pushed commit | n/a (read-from-git) | trailer line | contains `gardenio-agent-id: <bead-id>` (T2 §3 — recommended trailer) | same | n/a |
| GH-12 | Verified-signature badge (only meaningful on PAT-pool path with signing key registered) | Commit page "Verified" badge | same | `commit.verification.verified`, `verification.reason` | `false`, `unsigned` (App can't sign locally-pushed commits) | `true`, `valid` if pool member has signing key (else `false`) | `false`, `unsigned` |
| GH-13 | Push event actor | Repo events feed | `GET /repos/:o/:r/events?type=PushEvent` | matching event `actor.login` | `<app-name>` (push events show the App, not `[bot]` suffix in this field) | pool member | static |
| GH-14 | Audit-log actor (Enterprise Cloud only — skip on free org) | Org audit log | `GET /orgs/:o/audit-log?phrase=actor:<app-name>` | `actor` | `<app-name>` for App actions | pool member | static |
| GH-15 | Installation-token validity / TTL | Internal | `POST /installation/token` (issuance response) or `GET /installation/repositories` (smoke) | `expires_at`, 200 status | within 1h ± clock skew; refresh succeeds | n/a | n/a |

Notes:

- GH-1 through GH-3 are the load-bearing cells: they are the asymmetric
  GitHub case. The App-path PR's `user.login` will *always* be
  `<app-name>[bot]` — what we are *also* checking is that the
  PR body carries the `Created by agent: <bead-id>` annotation, which
  is the per-spawn signal a human can grep for in the UI. Both must be
  true for the per-agent rollout to be considered correct on GitHub.
- GH-12 (signed commits) is the only cell where the App and PAT-pool
  paths *differ in expected outcome*: App-path commits are `unsigned`
  (and that's by design — T2 §6.6 deferred signing to v1.1); PAT-pool
  commits are `Verified` if the pool member has a registered signing
  key. The matrix records the expected value per path; do not assert
  "always Verified."
- GH-11 (commit-message trailer) is recommended in T2 §6 implicitly
  via the body annotation discussion; we are calling it out here as a
  separate assertion. The trailer is the *machine-greppable* per-agent
  signal in the git log, complementary to the *human-readable*
  PR-body annotation. The fixture asserts both.
- GH-14 is gated on Enterprise Cloud (T2 §4) — skip when not on
  Enterprise. Same `expected_skip` pattern as GL-10.

### 2.3 Surfaces explicitly out of scope for v1

The following surfaces appear in T1/T2 but are *not* part of the v1
verification matrix (per T3 v1 scope):

- **Jira ticket comments / transitions.** T4 covered the Jira lifecycle
  *path*; the verification fixture for Jira lands when T4-Jira ships
  (currently deferred per T3 §1). When it does, the matrix grows a
  third provider with surfaces JR-1…JR-N analogous to GL-1…GL-12.
- **Slack / NATS / AWS / Anthropic / RWX / Mezmo** — all deferred per
  T3, no fixture work in v1.
- **Beads-daemon `created_by`.** Already verified by the existing
  per-agent-beads-token tests (T0 §7). The verification fixture uses
  the beads-daemon attribution as a *control* (it should already work
  before the per-agent provider tokens flip), but does not re-test it.

## 3. Fixture design

### 3.1 Where it lives (recommendation: RWX e2e)

The fixture lives at **`.rwx/per-agent-tokens-verification.yml`** in the
gardenio monorepo, alongside `.rwx/e2e.yml`. Reasoning:

- The wire layer (the only one that catches "the token actually
  produces the right upstream identity") needs *deployed* gardenio,
  *deployed* upstream issuer, and a *real* GitLab project + GitHub
  repo. RWX is where we already orchestrate the deployed-gardenio-rwx
  smoke tests; the per-agent-token assertion is the same shape.
- The Go-level controller e2e tests (in
  `gardenio/controller/test/...` if they were to exist for this) cannot
  reasonably hit GitLab.com / GitHub.com — they would mock both, and
  what we need to verify is precisely that the *unmocked* upstream
  produces the right field values. Mocks pass when production is
  broken.
- Splitting the fixture across both layers is possible — Go-level
  asserts the *contract* (issuer constructs the right call; lifecycle
  state machine transitions correctly), RWX asserts the *wire*
  outcome — and that's the recommended decomposition. Open Decision
  §6.1 surfaces it because it doubles the maintenance surface for
  marginal coverage gain over the wire layer alone.

### 3.2 Structure (provider-parameterised)

The fixture is a single RWX task that takes two parameters:

```
provider:        gitlab | github
mode:            per_agent | static
expected_actor:  <regex or literal>     # derived from provider + mode
```

For each `(provider, mode)` cell the task:

1. **Spawns a fresh disposable agent.** Uses `gb spawn --task <fixture-bead-id> --role per-agent-token-fixture` against a dedicated **test bead** (one new bead per fixture run, closed at end). Spawn nudge prompt: "Open a draft MR/PR titled `[fixture] per-agent-token-verification <run-id>` against `gardenio-test/fixtures` containing one trivial commit, then comment `[fixture] verification-comment` on it. Then exit."
2. **Waits for the agent to finish** (≤5 min). RWX polls `kd show <fixture-bead-id>` until status is `done` or timeout.
3. **Reads the upstream surfaces** (per §2.1 / §2.2). Uses the `gb token` CLI's *test admin* credential (different from the agent's per-agent token!) to query GitLab/GitHub APIs and dump JSON for each surface.
4. **Asserts each surface against `expected_actor`.** Match-or-fail per cell. Output is a single JSON report:
   ```json
   {
     "provider": "gitlab",
     "mode": "per_agent",
     "fixture_bead_id": "ka-XXXX",
     "results": [
       {"surface": "GL-1", "expected": "^project_\\d+_bot_[a-z0-9]+$", "actual": "project_42_bot_abc123", "pass": true},
       {"surface": "GL-2", "expected": "non-zero", "actual": 12345, "pass": true},
       ...
     ],
     "skipped": ["GL-10"],
     "summary": {"pass": 11, "fail": 0, "skip": 1}
   }
   ```
5. **Tears down.** Closes the fixture bead (cleanup-only), deletes the test MR/PR (or leaves it as `[fixture]`-prefixed and lets a janitor sweep — recommend the latter to keep tearing-down simple), and lets the per-agent-token revoke fire on `AgentDone`. The negative-test variants (§3.4) explicitly skip teardown to assert revocation correctness.

The "expected_actor" regex/literal table is a static config in the
fixture YAML — it does not depend on the spawn output, only on
`(provider, mode)`. This keeps the fixture deterministic.

### 3.3 What the agent does once spawned

The fixture agent's prompt is intentionally minimal — we want the
*spawning machinery* to be the system under test, not the agent's
ability to follow complex instructions. Per-provider:

**GitLab fixture role prompt:**
```
You are the per-agent-token verification fixture. Do exactly the
following and then exit.

  1. cd into the test repo at $TEST_REPO_PATH (already cloned by
     init-clone).
  2. Create a new branch named fixture/<bead-id>.
  3. Append the line "fixture run <bead-id>" to FIXTURE.txt.
  4. git commit -m "fixture: per-agent-token verification (<bead-id>)"
  5. git push origin fixture/<bead-id>
  6. glab mr create --target-branch main --title "[fixture] per-agent
     token verification <bead-id>" --description "Created by agent:
     <bead-id>" --draft
  7. glab mr note --message "[fixture] verification-comment" $(glab
     mr list --author=@me --output=json | jq -r '.[0].iid')
  8. exit cleanly via gb stop.

Do not push to main. Do not merge. Do not delete branches.
```

**GitHub fixture role prompt** (analogous, `gh` instead of `glab`):
```
... 1–5 as above with origin = github ...
  6. gh pr create --base main --head fixture/<bead-id> --title
     "[fixture] per-agent token verification <bead-id>" --body
     "Created by agent: <bead-id>" --draft
  7. gh pr comment <pr-number> --body "[fixture] verification-comment"
  8. exit cleanly via gb stop.
```

The fixture role's `secrets[]` declares `GIT_TOKEN`/`GIT_USERNAME`/
`GITLAB_TOKEN` (so the controller's per-agent path applies on top), and
its CLAUDE.md is a one-line file pointing at the prompt above. The role
is *non-interactive* — it does not ask for human input; it does the six
steps and exits.

### 3.4 Negative-path tests

Three negative cells per provider, run after the positive matrix
passes:

**N-1: Revoked token cannot perform actions.**

1. Spawn fixture agent as in §3.3.
2. After step 5 (push), but before step 6 (open MR), the fixture
   harness calls `gb tokens revoke <fixture-bead-id> --provider <P>`
   (per T4 §7.1).
3. The agent's step 6 (`glab/gh pr create`) must 401.
4. Fixture asserts: agent's exit code is non-zero AND the API call
   logged a 401 AND no MR/PR was created in the upstream.

**N-2: Expired token rotates cleanly (long-spawn case).**

1. Spawn fixture agent as in §3.3 but with a longer prompt that does:
   step 1–5 normally, then `sleep <rotation-window>`, then step 6.
2. The harness force-rotates mid-sleep (`gb tokens rotate
   <bead-id> --provider <P>` per T4 §7.1) — this re-mints the
   upstream token, rewrites the K8s Secret, and the kubelet projects
   the new value within ~60s.
3. Step 6 runs *after* rotation, succeeds, and the MR/PR is opened
   under the *same* per-agent identity (GitLab: same bot-user;
   GitHub: same App).
4. Fixture asserts: PR/MR created successfully AND `last_used_at` of
   the *new* token is non-null AND `last_used_at` of the *old* token
   is unchanged after rotation.

Recommend forcing the rotation rather than waiting for the natural 1h
GitHub TTL — running the fixture for >1h per cell is not feasible in
CI. The natural-TTL case is covered indirectly by the controller's
50-min re-mint loop being asserted correct in Go-level integration
tests (T6/T11 work, not this fixture).

**N-3: Fail-closed mode refuses to spawn on issuer outage.**

1. Set `PER_AGENT_TOKENS_FAIL_MODE=fail_closed` and
   `PER_AGENT_TOKENS_<P>=true`.
2. Block the controller's egress to the upstream issuer endpoint
   (e.g. via a NetworkPolicy applied to the controller's pod for the
   duration of this test, or via a feature flag in the issuer client
   that returns a synthetic 503).
3. Spawn fixture agent.
4. Fixture asserts: pod is not created AND the agent bead reaches
   `failed` state with a `provision-token` error message AND the
   metric `gardenio_agent_token_provision_failure_total{provider="<P>"}`
   incremented.
5. Restore connectivity.
6. (Sanity) Re-spawn agent with the same bead — pod is created, MR
   succeeds, all green.

For the fall-back-static counterpart of N-3 (set
`FAIL_MODE=fall_back_static`, block issuer): the fixture asserts the
pod *is* created AND the resulting MR's author is the *static*
identity AND
`gardenio_agent_token_provision_fallback_total{provider="<P>"}`
incremented. This is a positive-but-conditional test rather than a
negative test, but it lives next to N-3 because the only way to
trigger it is the same outage simulation.

### 3.5 Force-rotate hook

Negative test N-2 needs `gb tokens rotate <bead-id> --provider <P>`.
T4 §7.1 lists `rotate` as v1.1, not v1. **For the verification
fixture to land usefully in v1, `rotate` must move into v1** —
otherwise N-2 falls back to "wait an hour for natural TTL," which is
not viable in CI. Surfaced as Open Decision §6.5.

If operator opts to keep `rotate` in v1.1, an alternative is a
controller-side **test-only admin endpoint** that triggers a re-mint
for one bead (`POST /internal/test/agent-tokens/rotate?bead=<id>`),
guarded by a flag that's only on in `gardenio-rwx`. This is uglier but
keeps the surface out of the public CLI.

### 3.6 Cleanup contract

Each fixture run leaves these artifacts in the upstream:

- One closed-or-draft MR/PR (named `[fixture] per-agent-token
  verification <run-id>`).
- One pushed branch (`fixture/<bead-id>`).
- One commit (`fixture: per-agent-token verification (<bead-id>)`).
- Zero issues (the fixture comments on the MR/PR's intrinsic comment
  thread, not on a separate issue, to avoid creating a third
  resource).

Recommend a separate **fixture-cleanup task** that runs *before* each
fixture run and deletes any `[fixture]`-prefixed branches/MRs older
than 24h in the test repo. Idempotent, no harm if it runs against a
clean repo. (This pattern matches the existing `gardenio-rwx`
fixture-cleanup hooks for the e2e suite.)

The fixture bead (ka-XXXX created at spawn time) is closed by the
fixture itself in step 8 (`gb stop`) — controller revoke fires on
`AgentDone`, the K8s Secret is deleted, and the upstream PAT/
installation-token is revoked. The fixture harness asserts the K8s
Secret is gone and (for GitLab) the PAT is no longer listed in
`GET /api/v4/projects/:id/access_tokens` 60s after the agent exits.
This is the implicit revoke-test that piggybacks on every positive
run.

## 4. Feature-flag toggling matrix

The full matrix the verification suite runs per release:

| Cell | `PER_AGENT_TOKENS_GITLAB` | `PER_AGENT_TOKENS_GITHUB` | `FAIL_MODE` | What asserts |
|---|---|---|---|---|
| **GL-PA** | true | (any) | fall_back_static | GitLab surfaces (§2.1) match per-agent expected |
| **GL-ST** | false | (any) | fall_back_static | GitLab surfaces (§2.1) match static-mode expected |
| **GH-PA** | (any) | true | fall_back_static | GitHub surfaces (§2.2) match per-agent App-path expected |
| **GH-ST** | (any) | false | fall_back_static | GitHub surfaces (§2.2) match static-mode expected |
| **GL-N1** | true | (any) | fall_back_static | Negative N-1 (revoke mid-spawn) for GitLab |
| **GH-N1** | (any) | true | fall_back_static | N-1 for GitHub |
| **GL-N2** | true | (any) | fall_back_static | Negative N-2 (rotate mid-spawn) for GitLab |
| **GH-N2** | (any) | true | fall_back_static | N-2 for GitHub |
| **GL-N3-FC** | true | (any) | fail_closed | Negative N-3 fail-closed for GitLab |
| **GL-N3-FB** | true | (any) | fall_back_static | N-3 fall-back-static for GitLab (asserts static actor) |
| **GH-N3-FC** | (any) | true | fail_closed | N-3 fail-closed for GitHub |
| **GH-N3-FB** | (any) | true | fall_back_static | N-3 fall-back-static for GitHub |

12 cells per release. Each cell takes ~3–5 min wall-clock (spawn +
agent runs + assertions + cleanup), so the matrix is ~30–60 min serial
or ~10 min in parallel. Recommend running in parallel across two RWX
worker tasks (one per provider) with the negative cells serialised
within a provider.

The PAT-pool fallback path on GitHub (T2 §3.2) is *not* covered by
this matrix — it only lights up when a spawn opts in via
`spec.GithubTokenScope = "user"`, which v1 does not exercise by
default. When the pool path lands (v1.1), the matrix grows
**GH-PA-pool** cells parallel to **GH-PA**, asserting the leased pool
member's username instead of the App's `[bot]` actor.

### 4.1 Pre-merge gating

Add a **PR-gate** on the gardenio monorepo: any change touching
`agenttoken.go`, `agenttoken_gitlab.go`, `agenttoken_github.go`,
`internal/podmanager/initclone.go`, or
`internal/config/config.go`'s `PER_AGENT_TOKENS_*` plumbing must
trigger the verification matrix and pass before merge. This is one
new entry in `.gitlab-ci.yml`'s `rules` (file-glob match on those
paths). If the matrix is too slow to be a hard gate, file as
*advisory* (warn-only) for one release and promote to hard-gate
once the latency settles.

### 4.2 Per-release smoke

The release pipeline already runs `e2e.yml` against `gardenio-rwx`
before the prod-deploy gate. The verification matrix lands as an
additional dispatch in that same pipeline:

```
docker.yml: build → push → deploy gardenio-rwx → e2e.yml smoke →
  per-agent-tokens-verification.yml matrix → gate-prod-deploy
```

If the matrix fails, the prod deploy is gated. No emergency-bypass
escape hatch beyond the existing `GARDENIO_DEPLOY=1` flag (which the
operator must set manually).

## 5. Test data and isolation

### 5.1 Dedicated test resources

The fixture writes to upstream — it cannot use a shared "real" project
without polluting MR/issue dashboards. Recommend (Open Decision §6.2):

- **GitLab:** dedicated project `gardenio-test/per-agent-tokens-fixture`
  inside the `example-org` group. The controller's GitLab
  issuer credential needs `Maintainer` here; the fixture's per-agent
  bot users get `Developer`. The project is otherwise empty (one
  README, one `FIXTURE.txt` that the fixture appends to).
- **GitHub:** dedicated repo `acme/per-agent-tokens-fixture` (or
  whatever the org slug is). The org's GitHub App is installed on
  this repo (and only this repo, plus the existing gardenio repo for
  prod use). The fixture role's pool members (when v1.1 lights up the
  pool path) are added as collaborators with write access.

Alternative (rejected): reuse the production gardenio repos. **Rejected
because** every fixture run produces a `[fixture]` MR/PR that pollutes
the operator dashboards, and the bot-user list grows by one per spawn
on GitLab (the auto-created `project_NNNN_bot_xxxx` users from PAT
issuance), which is a real visible mess in `Project → Members`.
Dedicated test repos isolate this completely.

### 5.2 Test-fixture cleanup janitor

A separate scheduled cron task (daily at 03:00 UTC) closes any
`[fixture]`-prefixed MRs/PRs older than 24h in the test repos and
deletes their branches. This is one new RWX scheduled task, not new
infrastructure — it can reuse the existing `cron-bridge` mechanism if
that exists, or be a daily `gh`/`glab` script.

### 5.3 Fixture-bead labels

Every fixture spawn creates a bead labelled `fixture:per-agent-tokens`
so it's easy to grep `kd list --label fixture:per-agent-tokens` and
inventory what's been run. The bead's `created_by` is the controller's
identity (the fixture is dispatched by the harness, not by a human).
Beads are auto-closed by the fixture task in step 8.

## 6. Open decisions for operator review

These are the new T5 decisions, in addition to T1 §6 (seven items),
T2 §6 (eight items), T3 §3 (six items), and T4 §8 (six items). The
intake gardener should batch these into the post-T5 consolidated
`gb decision` per the bead's notes:

1. **Where do verification fixtures live — RWX e2e or gardenio/controller
   Go-level e2e?** Recommendation: RWX e2e for the wire layer; keep
   Go-level mocks for the issuer-call and lifecycle-state-machine
   contracts. Operator may prefer all-Go-level (faster, but mocks
   miss "the actual upstream returned the wrong field" bugs) or all-
   RWX (single home, but slower per-PR feedback). artifact_type:
   `plan`.
2. **Dedicated test resources or reuse production?** Recommendation:
   provision dedicated `gardenio-test/per-agent-tokens-fixture` GitLab
   project + dedicated GitHub repo. Operator may prefer reusing
   gardenio with a "fixture-only branch" convention to avoid
   provisioning toil. artifact_type: `checklist`.
3. **GitLab tier and audit-events surface (GL-10).** If we are on
   Free/Premium-without-audit-events, GL-10 `expected_skip`s. If we
   move to Ultimate, we should *un*-skip and assert. Operator confirms
   tier (mirrors T1 §6.1). artifact_type: `tldr`.
4. **GitHub tier and audit-log surface (GH-14).** Same as above for
   GitHub Enterprise Cloud (mirrors T2 §6.2). artifact_type: `tldr`.
5. **Promote `gb tokens rotate` to v1?** N-2 needs it; T4 §7.1 had it
   at v1.1. Operator picks: (a) promote `rotate` to v1 (clean),
   (b) ship a controller-side test-only admin endpoint behind a
   gardenio-rwx-only flag (uglier, smaller), (c) drop N-2 from v1
   verification (weakest). Recommendation: (a). artifact_type:
   `plan`.
6. **Fixture-PR cleanup retention.** 24h `[fixture]`-prefixed retention
   recommended; operator may prefer 7d (allows post-incident review)
   or aggressive `0h` (close immediately on fixture exit, no
   inspection window). artifact_type: `tldr`.
7. **Pre-merge hard gate vs advisory for the matrix.** Recommended:
   advisory for one release, then hard-gate. Operator may want
   hard-gate from day one (stricter) or advisory-forever (weaker).
   artifact_type: `tldr`.
8. **PAT-pool path verification (GitHub v1.1).** When the pool lands
   (T2 §3.2), the matrix grows GH-PA-pool cells. Out of scope for
   v1; flagged here so the matrix design accommodates it without
   re-architecting. No decision required for v1; operator
   acknowledges the future addition. artifact_type: `tldr`.

## 7. Pointers (for the T11/T12 fixture implementer)

| Topic | Where to look |
|---|---|
| Existing per-agent token issuer (the pattern under test) | `gardenio/controller/cmd/controller/agenttoken.go` |
| GitLab issuer call (T6 will add) | `gardenio/controller/cmd/controller/agenttoken_gitlab.go` (planned) |
| GitHub issuer call (T6 will add) | `gardenio/controller/cmd/controller/agenttoken_github.go` (planned) |
| Init-clone consumer (do not change) | `gardenio/controller/internal/podmanager/initclone.go:109–161` |
| `PER_AGENT_TOKENS_*` flag plumbing | `gardenio/controller/internal/config/config.go:189, :552` |
| Existing e2e dispatch pattern (RWX) | `.rwx/e2e.yml` |
| Existing release-gate pipeline | `.rwx/docker.yml`, `.gitlab-ci.yml` |
| `gb tokens` CLI (T10) | `gardenio/controller/cmd/gb/tokens.go` (planned) |
| `gb spawn` (existing) | `gardenio/controller/cmd/gb/spawn.go` |
| Beads-daemon HTTP client | `gardenio/controller/internal/beadsapi/client.go` |
| Predecessor: codebase state | `gardenio/docs/research/per-agent-tokens-codebase-state.md` |
| Predecessor: T1 GitLab survey | `gardenio/docs/research/per-agent-tokens-gitlab.md` |
| Predecessor: T2 GitHub survey | `gardenio/docs/research/per-agent-tokens-github.md` |
| Predecessor: T3 providers inventory | `gardenio/docs/research/per-agent-tokens-providers.md` |
| Predecessor: T4 lifecycle | `gardenio/docs/research/per-agent-tokens-lifecycle.md` |
| GitLab API: project access tokens | `https://docs.gitlab.com/api/project_access_tokens/` |
| GitLab API: merge requests (read author) | `https://docs.gitlab.com/api/merge_requests/#get-single-mr` |
| GitLab API: project events (push) | `https://docs.gitlab.com/api/events/` |
| GitHub API: pulls (read user) | `https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request` |
| GitHub API: list installation repos | `https://docs.github.com/en/rest/apps/installations#list-repositories-accessible-to-the-app-installation` |
| GitHub API: org audit log (Enterprise) | `https://docs.github.com/en/rest/orgs/audit-log` |

## 8. What this ADR does NOT decide

- The **issuer code** for each provider — that is T6 (gated on operator
  approval after T1–T5 land).
- The **`gb tokens` CLI implementation** — that is T10. This doc
  *uses* `gb tokens revoke` and (open-decision-permitting) `gb tokens
  rotate` in the negative tests; it does not specify the CLI shape
  beyond what T4 §7.1 already proposed.
- The **fixture implementation itself** — that is T11/T12, also gated
  on operator approval. T5 specifies *what the fixture asserts* and
  *where it lives*; the YAML and assertion code follow.
- The **cutover sequence** for flipping the per-agent-token sub-flags
  in production — that is T4 §9.2 and is unaffected by this doc.
- **Jira / other deferred providers' verification matrices** — out of
  scope per T3 v1.
- **The PAT-pool path verification on GitHub** (T2 §3.2 fallback) —
  v1.1 (Open Decision §6.8 acknowledges it).
- **Per-role permission templates** (T2 §3.4 / T1 §6.6) — when the
  issuer grows per-role scoping, the matrix grows per-role assertion
  cells. Out of scope for v1.
