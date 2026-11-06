# The Gardenio Lands GitHub App

Step 7.6 (gd-WsBvUZTzft) of the [Track B spec](../superpowers/specs/2026-10-19-lands-track-b-design.md),
B-D3 and §2.3. A Lands applicant proves they control the repository their land will work on by
installing this App on it and authorizing it as themselves. It is a **second App**, separate from
the fleet's `gardenio-io`, so a stranger's install can never reach the fleet's permission set or
its webhook.

| | Fleet App | Lands App |
|---|---|---|
| GitHub name | `gardenio-io` | `gardenio-lands` |
| Who installs it | the operator, on the operator's repos | any Lands applicant, on one repo each |
| `internal/githubapp` name | `githubapp.Fleet` | `githubapp.Lands` |
| Manifest | `MANIFESTS["agents"]` | `MANIFESTS["lands"]` |
| Token permissions | `AgentTokenPermissions` | `LandsTokenPermissions` |
| Webhook | the github-bridge's | none in v1 |
| Visibility | private | public |

Code: `gardenio/controller/internal/githubapp/` (`identity.go` names the two Apps, `user.go` is the
user-to-server leg, `setup.go` is the check) and `gardenio/controller/cmd/beads-viewer/lands_github.go`
(the two routes).

## Permissions

The whole permission set, and the set every tenant installation token requests:

| Permission | Access | Why |
|---|---|---|
| Contents | write | clone the repo, push a branch |
| Pull requests | write | open a pull request for the tenant to review |
| Issues | write | comment on the repo's issues |
| Checks | read | see whether a pull request went green |
| Metadata | read | GitHub grants it to every App; listed so the manifest says it |

Nothing else: no `actions`, `workflows`, `packages` or `administration`. A tenant agent cannot
rerun, cancel or edit CI and cannot change repo settings. Three places state this list and tests
keep them equal: `MANIFESTS["lands"]` in `gardenio/scripts/github-app-register.py`,
`githubapp.LandsTokenPermissions`, and this table.

## The manifest

The App is registered from code with the manifest flow, like the fleet's two Apps
([github-app-onboarding.md](../../plans/github-app-onboarding.md) P0), so the permission set is
reviewed and the private key never lands in a Downloads folder.
`./gardenio/scripts/github-app-register.py manifest --app lands --org mattbakerconsulting` prints it:

```json
{
  "name": "gardenio-lands",
  "url": "https://github.com/mattbakerconsulting",
  "description": "Gardenio Lands: agents work on the one repository you install this app on, opening pull requests for you to review. Install it on a single repository.",
  "redirect_url": "http://localhost:8731/callback",
  "public": true,
  "request_oauth_on_install": true,
  "setup_on_update": true,
  "default_permissions": {
    "contents": "write",
    "pull_requests": "write",
    "issues": "write",
    "checks": "read",
    "metadata": "read"
  },
  "default_events": [],
  "callback_urls": ["https://gardenio.clonk.tv/lands/github/setup"],
  "setup_url": "https://gardenio.clonk.tv/lands/github/setup"
}
```

- **`request_oauth_on_install: true`** is the point of the App: after installing, GitHub sends the
  user to the callback URL with `installation_id`, `setup_action` and a one-time `code` that only
  their own authorization produces.
- **`setup_on_update: true`** sends them back after they change the repository selection, so a
  wrong pick can be fixed and re-checked. GitHub sends no `code` on an update, so the setup
  handler sends the applicant through the App's OAuth authorize page once; that returns with a
  `code` and the `state`, and the `installation_id` waits in a short-lived cookie.
- **Public**, because a private App installs only on its owner's account. An install grants the
  installer nothing: no land exists until the operator approves an application (§2.5).
- **No webhook, no events.** Nothing in v1 reacts to installs.

## The setup check

1. `/lands/accept`, once the terms are accepted, offers **Connect your repository**
   (`GET /lands/github/install`). That sets a one-time state cookie and sends the applicant to
   `https://github.com/apps/gardenio-lands/installations/new?state=…`.
2. GitHub sends them back to `GET /lands/github/setup`. The viewer checks the state against the
   cookie and spends it, then `githubapp.VerifyLandsSetup`:
   exchanges the code for a user-to-server token; checks that token's GitHub user is the
   applicant's `github_id` claim; reads the installation with the App JWT (it must be the Lands
   App's own and not suspended); requires "Only select repositories" with exactly one repository
   the user can see; requires the user to be an admin of it; and requires its visibility to be
   public, or private while private repos are allowed. The user token is revoked at the end
   either way.
3. A pass sets a signed cookie (`lands_github`, 30 minutes, bound to the applicant's email and
   `github_id`, HMAC keyed from the App's client secret) that step 7.7's application form reads
   with `landsGitHub.verifiedFor`. The repository on an application comes from that cookie, never
   from the form.

Every refusal is a page that says what to do and offers **Start again**:

| Refusal | Status | When |
|---|---|---|
| unconfigured | 503 | the Lands App is not configured: **fail closed**, nothing reaches GitHub |
| nogithub | 403 | the sign-in carried no `github_id` (not a GitHub sign-in; step 7.3) |
| restart | 403 | no state cookie, or it does not match `state` |
| badrequest | 400 | `installation_id` or `code` missing or malformed, or an unknown `setup_action` |
| code | 403 | GitHub refused the code (expired, reused, forged) |
| wrong_user | 403 | the authorizing GitHub user is not the applicant |
| installation | 403 | not an installation of the Lands App, the user cannot see it, or the repo is outside the installation's account |
| suspended | 403 | the installation is suspended |
| repo_count | 403 | installed on all repositories, or not exactly one |
| not_admin | 403 | the user is not an admin of the repository |
| visibility | 403 | `internal`, or `private` with private repos off |
| unavailable | 502 | GitHub could not be asked; try again |
| requested | 200 | `setup_action=request`: an org member asked the owners to install |

Each row has a negative test: `internal/githubapp/setup_test.go` and
`cmd/beads-viewer/lands_github_test.go`.

## Configuration (beads-viewer)

| Env | Value |
|---|---|
| `LANDS_GITHUB_APP_ID` | numeric App id |
| `LANDS_GITHUB_APP_CLIENT_ID` | the App's OAuth client id |
| `LANDS_GITHUB_APP_SLUG` | `gardenio-lands` |
| `LANDS_GITHUB_APP_PRIVATE_KEY_PATH` | file holding the App private key (PEM) |
| `LANDS_GITHUB_APP_CLIENT_SECRET_PATH` | file holding the App client secret |
| `LANDS_GITHUB_APP_API_URL`, `LANDS_GITHUB_APP_WEB_URL` | optional overrides (tests) |
| `LANDS_GITHUB_ALLOW_PRIVATE_REPOS` | `false` refuses private repos; default allowed (owner's OQ2 answer, gd-IhEhhYTj4A) |

Secrets are files from a mounted Secret, never env values. With none of the first five set the
setup leg is off; with only some set the viewer logs an error and the leg stays off. Both answer
503.

## Chart (beads-viewer)

The Lands App leg and the plot application itself are two `beadsViewer` blocks in
`helm/gardenio/values.yaml` (decision gd-j3EiWECejv "chart"):

| Key | Renders |
|---|---|
| `beadsViewer.landsGithubApp.enabled`, `appId`, `clientId`, `slug` | the env above and the Secret file mount; `allowPrivateRepos: false` renders `LANDS_GITHUB_ALLOW_PRIVATE_REPOS=false`, `maxRepoSizeMB` renders `LANDS_MAX_REPO_SIZE_MB` |
| `beadsViewer.landsApply.enabled` | `LANDS_APPLY_ENABLED=true`: the form, the Gate and its reactor (step 7.7). Fails the render without `beadsViewer.machinePrincipal.enabled`, whose service account must hold `gardenio-machine-lands-gate` |
| `beadsViewer.landsApply.approver` | `LANDS_APPROVER` as `oidc:<email>`; empty means only gardenio-admins' answers count |
| `beadsViewer.landsApply.dailyLimit`, `defaultCredits` | `LANDS_APPLY_DAILY_LIMIT`, `LANDS_APPLY_DEFAULT_CREDITS` (empty = binary defaults 20 and 25) |
| `agents.mailer.*` with the Gate on | the same `MAILER_*` env and SES key files as the controller, so applicants get mail |

`/lands/apply` needs both blocks on: the Gate alone answers 503 because an application names a
repository the App verified. Rollback is `landsApply.enabled: false` (every `/lands/apply` route
answers 503, nothing else changes). The kbeads daemon and the viewer must come from the same
release: the reactor acts only on an answer the daemon stamped (`resolver_subject`,
`resolver_admin`), and an older daemon has no stamp.

## Operator rollout

1. Register the App: `./gardenio/scripts/github-app-register.py register --app lands --org mattbakerconsulting`.
   It stores `app-id`, `slug`, `client-id`, `private-key` and `client-secret` under
   `/gardenio/github-app/lands/` in SSM. Then `verify --app lands`.
2. Create the Secret `gardenio-lands-github-app` in the gardenio namespace with keys `private-key`
   and `client-secret` (by hand, or an ExternalSecret from those two SSM parameters).
3. Render the env above and mount the Secret on beads-viewer: in deploy-values set
   `beadsViewer.landsGithubApp.enabled: true` with `appId` and `clientId` from SSM
   (`/gardenio/github-app/lands/app-id` and `/client-id`); `slug`, `secretName`
   (`gardenio-lands-github-app`) and `mountPath` keep the chart defaults. The chart renders
   the five env values and mounts the Secret's `private-key` and `client-secret` as read-only
   files; it fails the render when `appId` or `clientId` is empty
   (`helm/gardenio/tests/lands_apply_test.go`).
4. The edge must let a signed-in applicant reach `/lands/github/*` (step 7.8), and the GitHub IdP
   must put `github_id` in the token (step 7.3); until both ship, the setup page answers
   `nogithub` or is not reachable.
