# GitHub App key rotation and revocation

Operating procedure for the two GitHub Apps registered in P0 of
[`gardenio/plans/github-app-onboarding.md`](../../plans/github-app-onboarding.md): the **agents**
App (`gardenio`) and the **ARC** App (`gardenio-ci`).

This runbook exists because §6 of that plan said to write it at P0 rather than discover its absence
in a year. Nothing in gardenio rotates any credential today — that is the actual starting
condition, and it is why the section on "what we do not have yet" below is as long as the
procedures.

## Why this key is different

An installation token is one hour and scoped to selected repositories. The **private key mints
unlimited installation tokens, for every repository the App is installed on, for as long as the key
is valid.** Its blast radius is strictly larger than the shared PAT it replaces. Treat a suspected
private-key leak as a cluster-level incident, not a credential refresh.

The ARC App's key is worse still in one specific way: it carries `administration:write`, which can
add and remove repository collaborators and delete runners. That is why it is a separate App
(D4) and why its key must never be placed in the `arc-runners` namespace, where runner pods execute
workflow code from any branch of four repositories.

## Where the material lives

| What | Where | Encryption |
|---|---|---|
| `app-id`, `slug`, `client-id`, `html-url` | SSM `/gardenio/github-app/<app>/<field>` | `String` — not secret |
| `private-key`, `webhook-secret`, `client-secret` | SSM `/gardenio/github-app/<app>/<field>` | `SecureString` under `alias/gardenio-github-app` |

`<app>` is `agents` or `arc`. The CMK is `alias/gardenio-github-app`
(`9b311963-9a58-4d0a-9586-b5ef20107f04`, us-east-1, account `123456789012`), created by CLI on
2026-09-21 with annual rotation enabled. Its key policy grants the account root full control and
`gardenio-administrator` encrypt/decrypt **only via SSM** (`kms:ViaService =
ssm.us-east-1.amazonaws.com`), so a stolen IAM session cannot use the key to decrypt anything it
carries out of Parameter Store by other means.

> **Drift:** the CMK is not in Terraform. `gardenio/terraform` is owned by another session lane, so
> it was created by CLI and is flagged here for that owner to codify. Until it is, a
> `terraform plan` will not know the key exists and will not recreate it if deleted.

All commands below assume `AWS_PROFILE=gardenio` and `us-east-1`.

## Routine: rotate a private key (no downtime)

GitHub allows **multiple private keys per App simultaneously**, which is what makes this a
generate → deploy → delete sequence with no window where the App cannot authenticate. Do not delete
the old key first.

1. **Generate.** GitHub exposes no REST endpoint for App private keys — generation and deletion are
   the App settings page: *Settings → Developer settings → GitHub Apps → `<app>` → Private keys →
   Generate a private key*. This downloads a `.pem`. It is the only step in this runbook that
   touches a browser download, and the file must not survive it.

2. **Store it, then destroy the download.**

   ```sh
   aws ssm put-parameter --name /gardenio/github-app/<app>/private-key \
     --type SecureString --key-id alias/gardenio-github-app --overwrite \
     --value "$(cat ~/Downloads/<app>.<date>.private-key.pem)"
   shred -u ~/Downloads/<app>.*.private-key.pem
   ```

   Prefer `--cli-input-json file://<tmp>` over `--value` if you are scripting this: `--value` puts
   the PEM in `argv`, where any other process on the box can read it from `/proc/<pid>/cmdline`.
   `gardenio/scripts/github-app-register.py` already does it the safe way and is the better path.

3. **Verify the new key before removing the old one.**

   ```sh
   gardenio/scripts/github-app-register.py verify --app <app>
   ```

   A `200` from `GET /app` proves the stored key signs for the stored app id. The same command
   diffs the App's live permissions against `MANIFESTS` in that script and lists installations, so
   it also catches permission drift introduced through the settings UI.

4. **Roll consumers.** Anything holding a materialized copy must be refreshed — as of 2026-09-21
   that is only the ARC Secret (P1), and only after the App is actually cut over:

   ```sh
   kubectl -n arc-runners delete secret gardenio-github
   kubectl -n arc-runners create secret generic gardenio-github \
     --from-literal=github_app_id="<id>" \
     --from-literal=github_app_installation_id="<installation id>" \
     --from-literal=github_app_private_key="$(aws ssm get-parameter \
        --name /gardenio/github-app/arc/private-key --with-decryption \
        --query Parameter.Value --output text)"
   helm upgrade ...   # all SIX scale sets — drive from `helm list -n arc-runners -o json`, not a written list
   ```

   The IDs **must be quoted strings** in that Secret — the ARC chart's own values file says so, and
   an unquoted integer is a silent auth failure. Note this is a **delete-and-recreate**, not an
   overwrite: the App form uses three different keys from the PAT form's single `github_token`, so
   the old key has to go or the Secret carries both.

5. **Delete the old key** on the same settings page. Only now.

6. **Update the inventory.** Close the superseded `type:credential` bead with `revoked_at` set, and
   create the replacement with `supersedes` pointing at it. The inventory is only worth having if
   this step is not skipped:

   ```sh
   kd --actor <you> --operated-by 'oidc:<you>' update <old-bead> -f revoked_at=<RFC3339>
   ```

## Routine: rotate the webhook secret

Only relevant once the App has a webhook endpoint (the github-bridge, see [`github-bridge.md`](github-bridge.md)); at P0 they are registered with no webhook.

`PATCH /app/hook/config` (authenticated with an App JWT) can set the webhook `secret`. Rotate the
SSM value and the receiver's copy in the same change — a mismatched secret means every delivery
fails signature verification, which P3's acceptance criteria require a test for in both the
bad-signature and empty-secret directions.

## Incident: suspected private-key compromise

The key mints tokens for every installed repository, so ordinary rotation is not sufficient — a
holder of the old key keeps minting until it is deleted.

1. **Delete the compromised key first**, on the settings page. This inverts the routine order
   deliberately: accept the outage. Every installation token already minted stays valid for up to
   an hour.
2. Generate and store a replacement (steps 1–3 above).
3. **Expire outstanding tokens** by suspending or uninstalling the installation — this kills live
   installation tokens immediately rather than waiting out the hour.
4. Audit `GET /app/installations` for installations you do not recognise. A **public** App is
   installable by any GitHub user with no approval queue on GitHub's side, so an unrecognised
   installation is a plausible finding once D2's public flip happens at P3, not a theoretical one.
5. File an escalation bead the same day. Per standing practice, escalate on first confirmation, not
   after a staleness clock.

## Incident: a leaked installation token

Much cheaper: one repository set, one hour, one agent.

- `DELETE /installation/token` revokes the token used to authenticate the call.
- Or have the user uninstall the App from their own settings page, which kills every live token for
  that installation with no operator involvement. That self-service revocation is one of the main
  reasons the plan chose an App over a PAT.

## What we do not have yet

State this plainly so nobody assumes otherwise:

- **No automated rotation.** Every step above is manual. Nothing schedules it, nothing reminds you.
- **The reminder is the credential inventory**, not this document. `type:credential` beads carry a
  real `expires_at` timestamp (operator decision `gd-NnaRPh0AY0`), so a query can find what is about
  to lapse. Nothing yet *runs* that query on a schedule — building that is the obvious next step and
  is not done.
- **The IMDS hole is open.** Every pod can reach IMDS and assume the node role, and the node role
  can read the admin kubeconfig from SSM (`gd-adirNxx3Qo`, netpol task `gd-fTCYDqMYeH`). While that
  is true, cluster placement of the key is not a real boundary — anything in the cluster can reach
  anything else. Keep the key out of the cluster until the netpol lands. **Flipping the IMDS hop
  limit to 1 is barred**: it breaks the autoscaler and `ecr-token-refresh`.

## The PAT cliff this replaces

Until P2 lands, three long-lived PATs remain, all inventoried as `type:credential` beads and all
verified against the live tokens on 2026-09-21:

| Token | Expires | Bead |
|---|---|---|
| `gardenio-crew-contents` (fine-grained; every agent pod) | **2026-10-19 09:29 UTC** | `gd-XCUtYCxe6N` |
| ghcr `read:packages` (ECR pull-through cache + fallback pull secret) | **2026-10-19 06:25 UTC** | `gd-swfgrU5atw` |
| ARC CI classic PAT (five runner scale sets) | 2026-12-14 20:03 UTC | `gd-OX3Px0lTm6` |

Two of the three lapse on the **same day**, about three hours apart — agent pushes and image pulls
through the cache would break together. The ARC token additionally holds near-total account scope
(`admin:org`, `admin:enterprise`, `repo`, `workflow`, `delete:packages`, …) rather than the
repo-admin the plan originally recorded, which is what makes P1 the highest-value phase.
