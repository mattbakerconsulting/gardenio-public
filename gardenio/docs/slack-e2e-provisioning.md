# Real-User Slack E2E — Provisioning & Session-Refresh Runbook

> Epic: [ka-0000006991](https://gardenio.e2e.example.com/beads/ka-0000006991) ·
> Task: [ka-0000002727](https://gardenio.e2e.example.com/beads/ka-0000002727) ·
> Plan: [`gardenio/plans/slack-e2e-playwright.md`](../plans/slack-e2e-playwright.md)

This runbook provisions the **dedicated test Slack workspace + app + human user**
that the real-user Playwright E2E harness (`gardenio/tests/slack-e2e/`, task
ka-0000004549) drives, and documents the recurring **session-refresh** procedure
for the browser cookies that expire.

It is split into two parts:

- **Part A — one-time provisioning (operator-only).** Creating a Slack
  workspace/app/user and writing values into AWS Secrets Manager cannot be done
  by the agent fleet: it needs Slack workspace-admin in a browser, and the
  in-cluster agent IAM role holds only `secretsmanager:GetSecretValue`/
  `DescribeSecret` — **no** `CreateSecret`/`PutSecretValue` (see
  `terraform/composition/canonical-prod/agent_irsa.tf`). These steps are the
  operator escalation this task files.
- **Part B — session refresh (recurring).** The captured session cookies expire
  on a rolling weeks-to-months cadence. When the `slack-ui` module starts
  failing auth, re-capture and rewrite one SM value in place — no redeploy.

Everything the **chart/terraform/harness** needs is already wired (this task):
the SM secret shells, the ESO ExternalSecret that syncs the session into the
ephemeral namespace, and the exact JSON contract. Part A/B only fills the
values.

---

## What gets created (inventory)

| Thing | Where | Created by | Notes |
|---|---|---|---|
| Test Slack **workspace** | slack.com | Operator (browser) | Free tier is fine; isolated from the prod gardenio workspace so tests never spam real channels |
| Test Slack **app** (Socket Mode) | api.slack.com/apps | Operator (browser) | Prod-equivalent scopes; its own `xoxb-`/`xapp-`/signing secret |
| Test **human user** | the test workspace | Operator (browser) | A real member Playwright authenticates as; NOT the bot |
| A channel for the tests | the test workspace | Operator (browser) | Invite the app; note the `C0…` channel ID |
| SM shell `slack-e2e/app-credentials` | `gardenio/prod/slack-e2e/app-credentials` | **Terraform** (this task) | Empty container; operator loads the app tokens |
| SM shell `slack-e2e/test-user-session` | `gardenio/prod/slack-e2e/test-user-session` | **Terraform** (this task) | Empty container; operator loads the captured storageState |
| ESO `ExternalSecret` → K8s `slack-e2e-session` | ephemeral namespace | **Chart** (this task) | Renders only when `externalSecrets.slackE2ESession.remoteRef` is set |

Both SM shells live under the `gardenio/prod/*` prefix, which is exactly the ESO
IRSA read scope on the consolidated prod-gardenio-eks cluster (the account e2e
runs against) — `secretStoreName: aws-secretsmanager`, prefix `gardenio/prod/*`,
no leading slash (`deploy-values/gardenio-rwx-ephemeral-prod.yaml.tmpl`).

---

## Part A — one-time provisioning (operator-only)

### A0. Prerequisites

- A Slack account you can create a **new free workspace** with (personal or a
  throwaway email; keep it separate from the prod gardenio workspace).
- The `gardenio` IAM user in account **123456789012** for the SM writes
  (advice ka-0000007102; keys in SM `gardenio/customer-poc-aws-creds`, fetched on
  the operator workstation only). `export AWS_DEFAULT_REGION=us-east-1`.
- Terraform already applied on `composition/canonical-prod` with the two
  `slack-e2e/*` names in `secrets_manager_logical_names` (this task) so the
  empty SM shells exist. Confirm:
  ```sh
  aws secretsmanager describe-secret --secret-id gardenio/prod/slack-e2e/app-credentials
  aws secretsmanager describe-secret --secret-id gardenio/prod/slack-e2e/test-user-session
  ```

### A1. Create the test workspace

At <https://slack.com/get-started> → **Create a new workspace**. Name it
something unambiguous, e.g. `gardenio-e2e`. Skip inviting anyone. This is a
throwaway workspace whose only purpose is automated tests.

### A2. Create the test Slack app (Socket Mode, prod-equivalent scopes)

At <https://api.slack.com/apps> → **Create New App** → *From scratch*, in the
`gardenio-e2e` workspace.

**OAuth & Permissions → Bot Token Scopes** (mirror prod gardenio — same set as
[`inbox-slack-enable-runbook.md`](inbox-slack-enable-runbook.md) §1):

```
chat:write, app_mentions:read, channels:history, channels:read,
groups:history, groups:read, im:history, im:read, im:write,
reactions:read, users:read, files:read, commands
```

> `commands` is included so slash-command E2E (Tier 2) works. If the harness
> later needs the assistant surface (Tier 3), also add `assistant:write` and
> enable the **Agents & AI Apps** feature.

**Socket Mode → Enable.** Then **Basic Information → App-Level Tokens →
Generate** a token with scope `connections:write` → this is the `xapp-…` token.
Socket Mode is what lets the ephemeral bridge attach with **zero inbound
ingress**.

**Event Subscriptions** (delivered over the Socket Mode WebSocket):
`app_mention`, `message.im`, `message.channels`, `member_joined_channel`.
Enable **Interactivity & Shortcuts** and **Slash Commands** so buttons/modals/
slash commands fire.

**Install to Workspace** → copy the **Bot User OAuth Token** (`xoxb-…`).
Grab the **Signing Secret** from *Basic Information* (Socket Mode does not need
it, but the chart accepts it and it is harmless to store).

You now hold three app values: `xoxb-…`, `xapp-…`, signing secret.

### A3. Create a channel and invite the app

In the `gardenio-e2e` workspace, create a channel (e.g. `#e2e`), then
`/invite @<your-app>`. Note the channel ID (`C0…`) via *View channel details*.
The harness (ka-0000006398) injects this at deploy time via
`--set slackBridge.slack.channel=C0…` — it is NOT stored in SM.

### A4. Create the dedicated test human user

In the workspace admin, **Invite people** and add a second account you control
(a throwaway email is fine). Accept the invite and finish onboarding so the user
is a full member. This is the identity Playwright drives — keep it distinct from
the bot and from your admin account.

### A5. Capture the test user's browser session (`storageState`)

Slack login is MFA/email-code gated and hostile to scripted form-fill, so we
capture the session **once, manually**, and inject it as Playwright's
`storageState`. Do this in a clean browser profile logged in **as the test
user** (A4), NOT your admin account.

```sh
# On the operator workstation, in a scratch dir (never commit these files):
npx playwright open --save-storage=storage-state.json https://app.slack.com/client
# A browser opens. Log in AS THE TEST USER (email code / MFA as prompted),
# land in the workspace, then CLOSE the browser. Playwright writes
# storage-state.json — a JSON doc {"cookies":[...],"origins":[...]} carrying
# the `d` and `d-s` session cookies.
```

Sanity-check the capture before storing it:

```sh
jq '.cookies | map(.name) | unique' storage-state.json   # must include "d" and "d-s"
```

> The `d` cookie is the long-lived Slack session token; `d-s` is its signed
> companion. Both are `HttpOnly` on `.slack.com` — Playwright captures them
> because it reads the browser context, not `document.cookie`.

### A6. Write the two SM values

Both are JSON with the exact properties the chart's ExternalSecrets extract
(`terraform/composition/canonical-prod/SECRETS.md`). A raw string fails the
sync.

```sh
export AWS_DEFAULT_REGION=us-east-1

# App tokens.
cat > slack-e2e-app-credentials.json <<EOF
{"bot-token":"xoxb-…","app-token":"xapp-…","signing-secret":"…"}
EOF
aws secretsmanager put-secret-value \
  --secret-id gardenio/prod/slack-e2e/app-credentials \
  --secret-string "$(cat slack-e2e-app-credentials.json)"

# Captured session — embed the storageState as a STRING property (`storage-state`).
jq -n --arg ss "$(cat storage-state.json)" '{"storage-state":$ss}' \
  | aws secretsmanager put-secret-value \
      --secret-id gardenio/prod/slack-e2e/test-user-session \
      --secret-string file:///dev/stdin

# Scrub the local plaintext.
shred -u slack-e2e-app-credentials.json storage-state.json 2>/dev/null || rm -f slack-e2e-app-credentials.json storage-state.json
```

### A7. Opt the harness in

The chart's `externalSecrets.slackE2ESession.remoteRef` is empty by default, so
nothing syncs until the E2E harness turns it on. When ka-0000006398 wires the
`slack-ui` module, set on the ephemeral overlay
(`deploy-values/gardenio-rwx-ephemeral-prod.yaml.tmpl`):

```yaml
externalSecrets:
  slackE2ESession:
    remoteRef: "gardenio/prod/slack-e2e/test-user-session"
  slackCredentials:
    remoteRef: "gardenio/prod/slack-e2e/app-credentials"
    includeSigningSecret: true
slackBridge:
  enabled: true
  slack:
    secretName: "slack-bot-credentials"   # materialized from slackCredentials.remoteRef
```

The ESO ExternalSecret then materializes the K8s Secret `slack-e2e-session`
(key `storage-state.json`) in the ephemeral namespace; the Playwright module
mounts it as its `storageState`. Absent the `remoteRef`, the module skips
cleanly (mirrors the `SLACK_BOT_TOKEN` guard on the existing bash slack
scripts), so local + fork CI + prod stay green.

---

## Part B — session refresh runbook (recurring)

The `d`/`d-s` cookies expire on a rolling cadence (weeks-to-months; Slack does
not publish an exact TTL). When they lapse, the `slack-ui` module fails at auth
(the browser lands on the Slack login screen instead of the workspace) — this is
the single most likely ongoing failure of this harness.

**Symptom:** `slack-ui` specs fail early with a login/redirect screen, or a
`globalSetup` assertion that the workspace loaded fails. The bot-token path is
unaffected (app credentials do not expire the same way), so *only* the
storageState-dependent specs break.

**Fix — re-capture and rewrite the one SM value:**

1. Re-run the capture (Part A5) as the test user → fresh `storage-state.json`.
2. Rewrite **only** `test-user-session` (app credentials are unchanged):
   ```sh
   jq -n --arg ss "$(cat storage-state.json)" '{"storage-state":$ss}' \
     | aws secretsmanager put-secret-value \
         --secret-id gardenio/prod/slack-e2e/test-user-session \
         --secret-string file:///dev/stdin
   shred -u storage-state.json 2>/dev/null || rm -f storage-state.json
   ```
3. The ExternalSecret re-syncs on its `refreshInterval` (category `oauth`,
   default **1h**) — no redeploy. To pull it immediately for an in-flight run,
   force a re-sync in the namespace:
   ```sh
   kubectl -n <ns> annotate externalsecret slack-e2e-session \
     force-sync="$(date +%s)" --overwrite
   ```

**Detecting expiry proactively:** `gardenio/tests/slack-e2e/session-guard.mjs`
is the liveness check. It fails with the exact line
`SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B` when the `d` /
`d-s` cookies are missing or past their `expires`, and warns when one expires
within 14 days. The `slack-ui` module's `globalSetup` runs it before any spec,
so a red run points straight here rather than looking like a flaky selector. It
is reusable outside the e2e runner (exit code `2` = expired):

```sh
cd gardenio/tests/slack-e2e
SLACK_E2E_STORAGE_STATE=storage-state.json npm run -s check-session           # offline
node session-guard.mjs --storage-state storage-state.json --probe --channel C0…  # + browser
```

`--probe` additionally loads the Slack web client headless and confirms the
workspace mounts (catches server-side revocation the cookie timestamps cannot
see). See the slack-e2e `README.md` for the Node API.

**Rotation hygiene:** re-capturing invalidates nothing on the Slack side (Slack
keeps prior sessions alive), but do keep the test user's password + email-code
access documented in the operator's password manager so any operator can refresh
it — this is a shared, recurring op, not a one-person secret.

---

## Cross-references

- Exact SM JSON shapes + seed commands:
  `terraform/composition/canonical-prod/SECRETS.md` (not in this repo — the
  canonical-prod terraform composition was excluded from the 2026-09-16 export)
- Prod Slack app scope list (mirrored here):
  [`inbox-slack-enable-runbook.md`](inbox-slack-enable-runbook.md) §1
- Chart wiring: `helm/gardenio/values.yaml` (`externalSecrets.slackE2ESession`,
  `externalSecrets.slackCredentials`) + `helm/gardenio/templates/external-secrets.yaml`
- Harness scaffold: task ka-0000004549 (`gardenio/tests/slack-e2e/`)
- Module wiring: task ka-0000006398 (`run-shard.sh` + `.rwx/e2e.yml`)
