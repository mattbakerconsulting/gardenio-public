# github-bridge runbook

The GitHub counterpart of `gitlab-bridge`: the GitHub App's webhook receiver and installation-token
minter. Source: `controller/cmd/github-bridge`. Chart: `helm/gardenio/templates/github-bridge/`.
Spec: bundle `gd-RwzAblfqeU`; plan `gd-j5OxXb3ySp`; onboarding plan
[`plans/github-app-onboarding.md`](../../plans/github-app-onboarding.md).

## What this slice does, and does not

Does:

- `POST /webhook` verifies `X-Hub-Signature-256` (HMAC-SHA256 over the raw body, constant-time,
  `sha256=` prefix required). It fails closed: no configured secret means the pod refuses to start,
  and the handler independently 403s if it ever ran without one.
- Drops replays: `X-GitHub-Delivery` ids are remembered for `REPLAY_TTL` (24h). A duplicate gets
  `200` and never reaches the event sink. A handler failure forgets the id, so GitHub's redelivery
  of the same GUID is processed instead of swallowed.
- Mints per-repo installation tokens with the existing `internal/githubapp` client, narrowed to the
  bridge permission set (`githubapp.BridgeTokenPermissions`: contents read, pull_requests write,
  issues write, checks read, actions read). It never requests `contents: write`.
- `/healthz`, `/readyz`, and `gardenio_github_webhook_deliveries_total{outcome}` on `:9101`.
- With `BEADS_HTTP_ADDR` set (the chart sets it), `check_suite` / `workflow_run` `completed` events
  are written onto the PR's merge-request bead, found by `mr_url` = `https://github.com/<repo>/pull/<n>`
  from the event's `pull_requests[]`. `conclusion` maps to the gitlab-bridge `mr_pipeline_status`
  vocabulary (`success`/`neutral` -> `success`; `failure`/`timed_out`/`startup_failure` -> `failed`;
  `cancelled`/`stale` -> `canceled`; `skipped`; `action_required` -> `manual`); `mr_pipeline_url` is
  the run's `html_url` (or the commit's checks page for a `check_suite`). The write emits
  `beads.bead.updated`, the shared event path agents and slack-bridge already subscribe to. When the
  verdict changes, a squawk-shaped message bead addressed to the producing task's assignee puts a
  one-line pass/fail in that agent's Slack thread (a URL-only change stays silent). Events with no
  PR (fork PRs, branch pushes), no conclusion, or no matching MR bead are logged and ignored.

- `pull_request` `opened`/`reopened`/`synchronize`/`closed`/`ready_for_review`/`converted_to_draft`/`edited`
  upsert the PR's merge-request bead (keyed by `mr_url`) with the shared `mr_*` fields plus
  `forge=github`: `mr_state` (`opened`/`closed`/`merged`, GitLab's enum), `mr_outcome`, `mr_iid`,
  `project_path`, branches, title, author. The work bead is found by (1) a task already carrying the
  PR URL, (2) the `agent/<role>/<bead-id>` branch, (3) bead ids in the PR body (probed, max 5). A PR
  that matches no bead is logged and ignored. The task gets `mr_url` + `forge` if it had no `mr_url`,
  and `produces`/`triggered-by` edges to the MR bead. The agent's thread gets a squawk-shaped line
  (opened, draft, reopened, ready, merged by X, closed without merging; `synchronize`/`edited` stay
  silent), and a still-active agent gets a normal-priority mail on closed/reopened. A reopened PR
  updates `mr_state` but does not reopen a closed MR bead.

- `pull_request_review` (`submitted`), `pull_request_review_comment` (`created`) and `issue_comment`
  (`created`, PR conversations only) are forwarded to the agent that owns the PR (a task bead whose
  `mr_url` is the PR). The agent's Slack thread gets the reviewer, a link to the exact comment and the
  quoted text (`path:line` for inline comments; approvals and change requests are labelled). The
  active agent gets a normal-priority mail with the full text, file/line and comment id, and the task
  bead gets `mr_has_review_comments=true` (not for approvals, not for a closed task). A `commented`
  review with no body is skipped: it is the envelope around inline comments, which arrive on their
  own. Comments from any `Bot` account or `[bot]` login (this App included) and any comment carrying
  the squawk marker are dropped, so agents never react to their own squawks. Only an author whose
  `author_association` is `OWNER`, `MEMBER` or `COLLABORATOR` is forwarded (gd-b5drtT0ymT): on a
  public repo anyone can comment on a PR, and their text would otherwise reach the agent as reviewer
  feedback. Anyone else's comment gets a `:warning: <login> (<association>, not a repo collaborator)`
  thread line with a link and no quoted text (thread lines are baked into later agents' prompts), no
  mail and no flag; a human relays it if it should be acted on. GitHub can report an org member whose
  membership is private as `CONTRIBUTOR`, which fails closed. gitlab-bridge's review
  nudge ignores `forge=github` beads so the flag write cannot nudge twice.
- Squawks: with an App configured, the bridge watches `beads.bead.closed` and posts a `gb squawk
  --to-mr` squawk (same opt-in as GitLab; plain squawks stay Slack-only) as a PR comment, using a
  per-repo installation token. The PR is the agent bead's `mr_url`, else its task bead's. Each comment
  ends with a hidden `<!-- gardenio-squawk:<hash> -->` marker (hash of PR + agent + text): the bridge
  checks the PR's comments for it before posting, so an SSE replay, a restart, or an agent repeating
  itself posts once. The event stream starts from "now", so a squawk closed while the bridge was down
  is not posted.

- Resolver: with an App and a beads daemon, a sweep (every `RESOLVER_INTERVAL`, default 5m, first
  run one interval after boot) respawns a worker for unresolved review threads when the PR's agent is
  gone. Candidates are unfinished work beads (open/in_progress/blocked) whose `mr_url` is a GitHub PR
  and that carry `forge=github` or `mr_has_review_comments=true`. Guards, cheapest first, so GitHub is
  only asked about a PR once everything local says a respawn is possible: a live agent on the task or
  the PR (not closed, `agent_state` not done/failed, not stalled past `ProgressStaleThreshold`) means
  it is left alone; a per-task cooldown (`RESOLVER_COOLDOWN`, default 30m); a task nobody ever worked
  (nobody to continue); then GraphQL `reviewThreads { isResolved }`, skipping PRs that are not OPEN or
  have no unresolved thread. A thread whose first comment's `authorAssociation` is not `OWNER`,
  `MEMBER` or `COLLABORATOR` is not counted, so an outsider's thread can neither trigger a respawn nor
  appear in its prompt. At most `RESOLVER_MAX_RESPAWNS` (default 3) respawns per set of
  unresolved threads (a new thread resets the count), at most 3 spawns and 50 GitHub lookups per
  sweep, and the spawn itself carries `single_claim_task` so the daemon refuses a second crew. The
  cooldown, count and thread-set hash live on the task bead (`resolver_spawned_at`,
  `resolver_spawn_count`, `resolver_threads_hash`), so a restart or a second replica cannot forget a
  spawn. The respawn reuses the task's last agent name (session continuity), and its first prompt lists
  the threads with links. The agent's Slack thread gets `:recycle: N unresolved review threads on <PR>
  and <agent> is no longer running. Respawned it (attempt k of 3)` with the first threads quoted; when
  the cap is hit it gets one `:warning: ... needs a human` line instead and the resolver stops for that
  thread set. `RESOLVER=off` disables it; `RESOLVER_DRY_RUN=true` logs what it would do and writes
  nothing. Needs `pull_requests: read` (the bridge token already has write). Unlike gitlab-bridge's
  event-driven resolver this is a sweep, so it also catches threads left open after an earlier round
  and review events missed while the bridge was down.
- Draft agent PRs (plan gd-WFMvg9sDKE task 1.4, off by default): with
  `GITHUB_BRIDGE_DRAFT_AGENT_PRS=true` (chart `githubBridge.draftAgentPRs`) and an App, a PR
  `opened` as ready from an `agent/*` branch onto `main` is converted to a draft
  (`convertPullRequestToDraft`, needs `pull_requests: write`) and gets one comment telling the
  agent to run `gb pr ready`, so it runs the draft CI tier until then. A failed conversion is
  logged and the event is mapped as usual; a redelivery finds the comment's marker and does not
  comment twice.
- Deployments (plan gd-UZKZ9H25UD step 4b, `deployment.go`): with the release recorder on (it
  needs the bridge's machine principal, `BEADS_OIDC_*`), a `deployment_status` of state `success`
  becomes a deployment row on a release of the project whose `git_url` is the repo: the release the
  deployment's `ref` names as a tag (recorded first if the tag webhook has not landed), else the
  newest of the project's last 50 releases at its `sha`. The target is the deployment's
  `environment`, `via=github`, `by=github:<deployment creator>`, `revision` the deployment id,
  `since` the status's `created_at`. An environment the project does not declare is added to its
  `release_targets` by the daemon as kind `github-environment`. `failure`, `error`, `inactive` and
  the in-between states write nothing, nor does a deployment with no recorded release (logged).
  Needs the App's `deployments: read` and the `deployment_status` event (escalation gd-ShFmcrAXy5).

Events no slice handles go to a sink that logs `event/action/repo` only (never payloads).

## Retiring the PAT cliff (2026-10-19)

The crew PAT (`gardenio-crew-contents`) and the ghcr `read:packages` token both lapse on
2026-10-19. This bridge removes the *webhook and API-read* dependence on a PAT: inbound auth is the
App's webhook secret, outbound API auth is a one-hour installation token. It does **not** by itself
replace the PAT agents push with — that is the per-agent installation-token work
(`agents.githubApp`, epic `gd-BHLDnjX1Hw`). Both halves must land before 2026-10-19 for the cliff to
be gone.

## Enabling it

For the k3s cluster, follow [Enabling it on k3s](#enabling-it-on-k3s-operator) under Wake on CI
instead: it has no External Secrets Operator, and the App is already registered.

1. Register the App with a webhook and events (the one operator click):
   `gardenio/scripts/github-app-register.py register --app agents --org gardenio-io --webhook-url https://<host>/webhook`.
   With `--webhook-url`, the manifest subscribes to `pull_request`, `pull_request_review`,
   `pull_request_review_comment`, `pull_request_review_thread`, `issue_comment`, `check_suite`,
   `workflow_run` and `deployment_status`. Without it the App stays inert (P0). GitHub returns the webhook secret in the
   conversion response and the script stores it at `/gardenio/github-app/agents/webhook-secret`.
2. Deploy values (PR to `deploy-values/`, never `helm upgrade` from a pod):

   ```yaml
   githubBridge:
     enabled: true
     webhookSecret:
       externalSecret: { enabled: true, secretStoreName: <ssm-store> }
     githubApp:
       appId: "<agents app id>"        # omit to receive webhooks without minting tokens
     ingress:
       enabled: true
       host: github-webhook.<domain>
       entryPoints: [webhooks]         # whatever the public, non-AVA entrypoint is
   ```

   The private key Secret is the one `agents.githubApp.externalSecret` already syncs.
3. Check it: App settings -> Advanced -> Recent Deliveries -> the `ping` should be `200`.
   `gardenio_github_webhook_deliveries_total{outcome="ping"}` increments.

## Wake on CI (`gb wait ci`)

The bridge resolves the CI waits agents record with `gb wait ci` and wakes the waiting agent
when its runs conclude (`ciwait.go`, `ciwait_poll.go`, `ciwake.go`). How it works, and how to
diagnose a wait: [`triggers.md`, "Waking an agent when its CI concludes"](../triggers.md#waking-an-agent-when-its-ci-concludes-gb-wait-ci).
Plan: [`plans/wake-on-ci-completion.md`](../../plans/wake-on-ci-completion.md) (epic `gd-j4OrJOprh9`).

| Env | Default | Meaning |
|---|---|---|
| `CI_WAIT_RESOLVER` | on | `off` disables the resolver. It needs only `BEADS_HTTP_ADDR`, not an App. |
| `CI_WAIT_SWEEP_INTERVAL` | `15s` | How often pending waits are listed and evaluated. |
| `CI_WAIT_QUIESCE` | `60s` | How long a sha's runs must be unchanged, all completed, before the wait resolves. |
| `CI_WAIT_POLL_GRACE` | `2m` | First Actions poll for a waiter after this. Needs the App. |
| `CI_WAIT_POLL_INTERVAL` | `5m` | Actions poll interval per waiter after the first. |

The chart has no values for these yet; the defaults are what the plan asks for.

### State on k3s (2026-11-01)

- **github-bridge is not running.** `deploy-values/gardenio-k3s.yaml` sets only
  `githubBridge.machinePrincipal.enabled`; `githubBridge.enabled` is still the chart default,
  `false`.
- **The App's webhook points at trigger-engine, not here.** The `gardenio-io` agents App
  (id `5029022`, installation `170295668`) delivers `workflow_run` and `check_suite` to
  `https://hooks.clonk.tv/webhooks/<token>`, the trigger-engine route, where trigger
  `gd-Ud9UPjEsmy` (action `notify`) receives them and does nothing with them
  (escalation `gd-fyCexuAP9d`, 2026-10-11).
- So no resolver runs. A parked agent is revived only by the 25-minute `next_respawn_at`
  timer `gb stop` stamps, and its `gb wait ci --result` asks GitHub itself.

### Enabling it on k3s (operator)

These are operator steps: `deploy-values/**`, Secrets and the App settings are outside the
crew lane. Do them in this order, so the App never points at a route with nothing behind it.

1. **Seed two Secrets by hand.** k3s has no External Secrets Operator (`externalSecrets` is
   off), so leave `githubBridge.webhookSecret.externalSecret` disabled.
   - Webhook secret: the value the App already sends, which `gd-fyCexuAP9d` stored in SSM at
     `/gardenio/github-app/gardenio-io/webhook-secret` (the same value as trigger
     `gd-Ud9UPjEsmy`'s `webhook_secret`). Keep the value; only the URL changes in step 3.

     ```sh
     kubectl -n gardenio create secret generic gardenio-github-webhook-secret \
       --from-file=webhook-secret=<(aws ssm get-parameter --with-decryption \
         --name /gardenio/github-app/gardenio-io/webhook-secret \
         --query Parameter.Value --output text)
     ```

   - App private key, for the poll fallback, the squawk poster and the release recorder:
     the bridge mounts Secret `gardenio-github-app-private-key`, key `private-key`. Check
     whether it exists first (`kubectl -n gardenio get secret gardenio-github-app-private-key`).
     If not, create it the same way from the agents App's `private-key` parameter. The key
     rotation runbook names it `/gardenio/github-app/agents/private-key`; `gd-fyCexuAP9d`
     used `gardenio-io` as the path segment, so check which one holds it.
     Without this Secret the pod cannot mount it and does not start. To run without it,
     leave `githubApp.appId` unset: the resolver then runs webhook-only (no poll fallback).
2. **Deploy values** (a PR to `deploy-values/gardenio-k3s.yaml`, merged, then rolled at the
   next release window with `release-deploy.sh`; never `helm upgrade` from a pod). Replace
   the existing `githubBridge:` block with:

   ```yaml
   githubBridge:
     enabled: true
     machinePrincipal:
       enabled: true
     # The gardenio-io agents App (5029022), installed on mattbakerconsulting/gardenio.
     # Mints the Actions-read token the CI-wait poll fallback uses.
     githubApp:
       appId: "5029022"
       installationId: "170295668"
     # No ingress: block. hooks.clonk.tv resolves to the public edge, not Traefik.
   ```

   The route GitHub reaches is not in the chart. `hooks.clonk.tv` resolves to the public-edge
   nginx (`gardenio/helm/gardenio-ziti/public-edge.yaml`, cost plan C4), whose
   `location = /webhook` proxies POSTs from GitHub's `hooks` ranges to
   `gardenio-github-bridge.gardenio.svc.cluster.local:8101` (gd-blqlgxJmap), next to
   `/webhooks/` for trigger-engine. The chart's `githubBridge.ingress` IngressRoute sits on
   Traefik's load balancer, which no DNS name points at, so on k3s it is a no-op. The live
   values still set `ingress.enabled: true`; it is harmless and can go back to `false` in a
   later values PR (the nginx allowlist does what `ipWhitelist` would). The edge manifest is
   applied by hand (`kubectl apply -f gardenio/helm/gardenio-ziti/public-edge.yaml`, then
   `kubectl -n public-edge rollout restart deploy/public-edge`), and the edge probe expects
   `POST /webhook` from a non-GitHub address to be `403`.

   Check after the roll:
   - the pod is Ready;
   - its log has `ci wait resolver enabled ... poll_fallback=true`;
   - `curl -si -X POST https://hooks.clonk.tv/webhook` from a laptop returns `403` with
     `server: nginx`: the edge route exists and refuses a non-GitHub address.
     `gardenio_github_webhook_deliveries_total{outcome="bad_signature"}` must not move. To
     prove the bridge itself answers, port-forward to the pod
     (`kubectl -n gardenio port-forward deploy/gardenio-github-bridge 8101`) and POST to
     `localhost:8101/webhook`: `403`, and that counter increments. The end-to-end proof is the
     redelivery in step 3.
3. **Point the App webhook at the bridge.** In the App's settings
   (`https://github.com/settings/apps/gardenio-io`), set Webhook URL to
   `https://hooks.clonk.tv/webhook`. Leave the secret, content type (`application/json`) and
   events (Workflow run, Check suite) as they are. Then Advanced, Recent Deliveries:
   redeliver the newest delivery and expect `200`.
4. **Retire the trigger-engine intake.** Trigger `gd-Ud9UPjEsmy` no longer receives
   anything: `kd update gd-Ud9UPjEsmy -f enabled=false`. Leave the trigger-engine ingress
   alone; other webhook triggers may use it.
5. **Prove it end to end.** From an agent that has pushed a commit:
   `gb wait ci --repo mattbakerconsulting/gardenio --sha <full sha>`, then `gb stop`. It
   should be resumed with `wake_reason=ci` within about two minutes of the last run
   concluding (the 60 s quiesce plus a 15 s sweep), and the bridge log should show
   `ci wait concluded` then `ci wait: agent woken`. This is plan step P6 (`gd-q4KMd2egqF`).

What else turns on. With the App configured, the bridge also starts these, and the chart
has no switch to stop them:

- the squawk poster (`gb squawk --to-mr` becomes a PR comment);
- the review-thread resolver, which respawns a worker for unresolved review threads when the
  PR's agent is gone. It respawns at most 3 times per thread set. `RESOLVER_DRY_RUN=true`
  only logs, but there is no chart value for it;
- the release recorder (`machinePrincipal` is already on for it).

With or without the App, it also runs the `check_suite`/`workflow_run` bead sink, which
writes `mr_pipeline_status` onto PR merge-request beads and posts a pass/fail line in the
producing agent's Slack thread when the verdict changes.

Only `workflow_run` and `check_suite` are subscribed today. The `pull_request` and review
slices stay idle until the App also subscribes to those events, which is a separate call.

Rollback: point the App webhook back at the trigger-engine URL (trigger `gd-Ud9UPjEsmy`'s
`webhook_token`, re-enabled), then set `githubBridge.enabled: false` at the next window.
Nothing is lost: pending waits fall back to the 25-minute timer.

## Reading the metric

| `outcome` | Meaning |
|---|---|
| `accepted` | Verified, new, handled. |
| `duplicate` | Verified replay (or GitHub redelivery of a handled GUID); ignored with 200. |
| `ping` | The App's test delivery. |
| `bad_signature` | Wrong/missing signature. A sustained stream is a mis-set secret or a scanner. |
| `bad_request` | Wrong method, content type, or missing delivery/event header. |
| `no_secret` | Handler ran with an empty secret. Should be impossible; page on it. |
| `handler_failed` | Sink returned an error; 500 sent, id forgotten, GitHub can redeliver. |
| `replay_cache_full` | 100k live ids; shedding with 503 rather than evicting a live id. |

## Known limits (honest)

- **`mr_pipeline_status` is last-completed-wins.** A repo with several workflows per PR can show
  `failed` then `success` as unrelated runs finish, and out-of-order deliveries are not reordered
  (GitLab's single pipeline per MR has no such ambiguity). The thread line announces each verdict
  change, so a failure is never silent, but the bead field reflects only the latest run.

- **The replay cache is in memory.** A restart forgets it, so a captured, validly signed delivery can
  be replayed once after a restart within the TTL. Sinks must therefore be idempotent (bead field
  updates are). The Deployment is `Recreate` with one replica so two pods never disagree. Persisting
  it is a possible later slice; GitHub offers no timestamp header to bound replay otherwise.
- **One webhook secret.** Rotating it has a brief window where deliveries fail signature
  verification (SSM synced, GitHub not yet updated, or vice versa). Redeliver the failures from the
  App's Advanced tab. See also the webhook-secret section of
  [`github-app-key-rotation.md`](github-app-key-rotation.md).
- Source-IP allowlisting is available (`githubBridge.ingress.ipWhitelist`) using the `hooks` list
  from `https://api.github.com/meta`, but is off by default: the list changes and a stale one
  silently drops real deliveries. HMAC is the authentication.
