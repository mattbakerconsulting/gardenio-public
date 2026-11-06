# Slack-enable a gardenio-in-a-box — Operator Runbook

> Plan bead: [ka-0000005588](https://gardenio.e2e.example.com/beads/ka-0000005588) ·
> Deliverable bead: ka-0000005227 ·
> Overlay: `deploy-values/gardenio-inbox-slack.yaml` (decommissioned — excluded from this export)

This turns on Slack control of an existing gardenio-in-a-box (EC2 k3s) using
**Socket Mode** — an *outbound* WebSocket from the box to Slack. The box gets
**zero new inbound surface**: no public webhook, no extra ingress, no inbound
443 from Slack. You @mention or DM the gardenio app → it spawns/answers agents;
bead lifecycle + decisions post back to Slack.

---

## 0. The target box

The gardenio-in-a-box lives in the **AWS-poc account (123456789012)** and is
driven by an operator over SSH (the in-cluster fleet's AWS role is in the mgmt
account and cannot reach it — by design). Known live box from prior work:

| Box | EIP | Instance | State |
|---|---|---|---|
| **gardenio-in-a-box** (recommended target) | `192.0.2.91` | full stack, app `2026.160.13`, on-box k3s | **live**, 443 reachable |
| tarboat-proof (blank) | `3.224.147.106` | blank AL2023, 8vCPU/61GB | scratch box, no stack |

> Confirm the live box is still the one you want before wiring. If you spun up a
> fresh box, substitute its EIP/namespace below. Default namespace: `gardenio`.

Everything in §1–§4 runs **on the box** (SSH in; the box already has `kubectl`
shimmed to `k3s kubectl` and the gardenio repo checked out — typically under
`/opt/gardenio/src` or your clone dir; adjust `REPO` below).

---

## 1. Create the Slack app (off-box, once)

At <https://api.slack.com/apps> → **Create New App** → *From scratch*.

**OAuth & Permissions → Bot Token Scopes:**
`chat:write`, `app_mentions:read`, `channels:history`, `channels:read`,
`groups:history`, `groups:read`, `im:history`, `im:read`, `im:write`,
`reactions:read`, `users:read`, `files:read`

**Socket Mode → Enable** (this is what removes the need for a public webhook).
Then **Basic Information → App-Level Tokens → Generate** a token with scope
`connections:write` → this is the `xapp-…` token.

**Event Subscriptions** (Socket Mode delivers these over the WebSocket):
`app_mention`, `message.im`, `message.channels`, `member_joined_channel`.
Enable **Interactivity & Shortcuts** and **Slash Commands** (Socket Mode) so
modals/buttons work.

**Install to Workspace** → copy the **Bot User OAuth Token** (`xoxb-…`).
Grab the **Signing Secret** from *Basic Information* (optional — only used if we
ever fall back to webhook mode; harmless to include).

**Invite the app to a channel** and copy its **channel ID** (`C0…`):
channel name → *View channel details* → bottom of the dialog.

You now hold three values: `xoxb-…` (bot), `xapp-…` (app), signing secret;
plus a `C0…` channel ID.

---

## 2. Create the K8s secret on the box (tokens never go in git)

```sh
NS=gardenio
kubectl -n "$NS" create secret generic gardenio-slack-bridge-creds \
  --from-literal=bot-token='xoxb-…' \
  --from-literal=app-token='xapp-…' \
  --from-literal=signing-secret='…'     # optional; include if you have it
```

> The chart **rejects** plaintext tokens in values (`AUDIT §J6`) and reads only
> from this secret by name. Keys must be exactly `bot-token`, `app-token`,
> `signing-secret`.

---

## 3. Provide the channel ID at deploy time (no overlay edit)

`deploy-values/gardenio-inbox-slack.yaml` ships with an **empty**
`slackBridge.slack.channel` on purpose — it stays reusable across boxes. Inject
the real per-box `C0…` id at deploy time instead of editing the file. Every
layer of the install path accepts it (`ka-0000005575`):

| Path | Flag |
|---|---|
| `scripts/render-user-data.sh` (fresh box) | `--slack-channel C0XXXXXXXXX` |
| `scripts/bootstrap-ec2.sh` | `--slack-channel C0XXXXXXXXX` |
| `scripts/deploy-byo.sh` (re-deploy on the box) | `--slack-channel C0XXXXXXXXX` |
| plain `helm upgrade` | `--set slackBridge.slack.channel=C0XXXXXXXXX` |

If you forget it, the bridge comes up with **no** channel (fail-safe) rather
than a bogus placeholder — just set it and re-deploy. Optionally also set
`beadsWebURL` so Slack permalinks resolve:
`--set slackBridge.beadsWebURL=https://gardenio.<BOX_IP>.sslip.io/beads`.

---

## 4. helm upgrade with the Slack overlay layered LAST

The in-box profile is the base chart + two in-box overlays; add this one **last**
so it wins precedence. Two equivalent paths:

**A. Direct helm (matches the install runbook's overlay order):**

```sh
REPO=/opt/gardenio/src          # adjust to your clone
cd "$REPO"
helm upgrade --install gardenio gardenio/helm/gardenio \
  --namespace gardenio --create-namespace \
  --values gardenio/helm/gardenio/values.yaml \
  --values gardenio/helm/gardenio/values-inbox.yaml \
  --values deploy-values/gardenio-inbox.yaml \
  --values deploy-values/gardenio-inbox-slack.yaml \
  --set slackBridge.slack.channel=C0XXXXXXXXX \
  --wait --timeout 10m
```

**B. Via the BYO/bootstrap wrapper (`--extra-values` is repeatable, applied in order):**

```sh
cd "$REPO"
scripts/deploy-byo.sh --namespace gardenio \
  --values deploy-values/gardenio-byo.yaml \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox-slack.yaml \
  --slack-channel C0XXXXXXXXX
```

> Use whichever overlay stack your box was originally installed with — keep the
> first overlays identical to your install and just append the slack overlay.

> ⚠️ **`--wait` hangs if *any* sibling deployment is unhealthy.** `helm upgrade
> --wait` blocks until **every** Deployment in the release is Ready — not just
> the ones this overlay touches. If a pre-existing deployment is crashlooping
> (e.g. `gardenio-beads-viewer` behind a netpol gap), the upgrade hangs the full
> `--timeout` then rolls back, even though the slack-bridge/controller rollout
> itself succeeded. If you know a sibling is already broken, run the upgrade
> **without `--wait`** and watch the rollout you care about by hand:
> ```sh
> helm upgrade --install gardenio gardenio/helm/gardenio -n gardenio ... # (no --wait)
> kubectl -n gardenio rollout status deploy/gardenio-slack-bridge --timeout=120s
> kubectl -n gardenio rollout status deploy/gardenio-agents       --timeout=120s
> ```
> Fix the crashlooping sibling separately — don't let it gate the reconcile.

> ⚠️ **Out-of-band resources block adoption.** If a `slack-bridge` Deployment
> (or its `gardenio-slack-bridge-state` PVC) was created by hand — e.g.
> `kubectl apply` on top of a release that had `slackBridge.enabled=false` — it
> lacks Helm ownership metadata and the upgrade fails with an adoption
> collision ("exists and cannot be imported / not owned by the release"). Stamp
> the ownership annotations so Helm adopts them, then re-run the upgrade:
> ```sh
> for kind in deployment/gardenio-slack-bridge pvc/gardenio-slack-bridge-state; do
>   kubectl -n gardenio annotate "$kind" \
>     meta.helm.sh/release-name=gardenio \
>     meta.helm.sh/release-namespace=gardenio --overwrite
>   kubectl -n gardenio label "$kind" app.kubernetes.io/managed-by=Helm --overwrite
> done
> ```
> Root cause to avoid the patch next time: turn the bridge on **through Helm**
> (`slackBridge.enabled=true`, §4) rather than `kubectl apply`-ing it alongside a
> release that has it disabled.

---

## 5. Map the channel → project (so messages route)

On the project bead, add the channel id to the `slack_channel` field (a
comma-separated list) so messages in that channel route to `gardenio`. Use
`--field-append`, which adds ONE channel **without clobbering** the channels
already listed:

```sh
kd update <gardenio-project-bead> --field-append slack_channel=C0XXXXXXXXX
# (DMs fall back to slackBridge.defaultDMProject=gardenio regardless.)
```

> ⚠️ **Do NOT use bare `-f slack_channel=…` here.** `slack_channel` is a SHARED
> AGGREGATE field (a CSV that multiple operators/agents append to). A bare
> `-f slack_channel=C0XXXXXXXXX` REPLACES the entire value, silently dropping
> every channel already mapped to the project. The non-clobbering edit flags
> (added for ka-0000007767) are the safe path:
>
> | Intent | Safe flag | What bare `-f` does instead |
> |---|---|---|
> | add one channel | `--field-append slack_channel=C0…` | replaces the whole CSV |
> | remove one channel | `--field-remove slack_channel=C0…` | replaces the whole CSV |
> | set one channel's role override (JSON map) | `--field-set channel_roles.C0…=<role>` | replaces the whole map |
> | set MULTIPLE roles on one channel (ka-0000006075) | `--field-set channel_roles.C0…="<role-a>,<role-b>"` (comma-separated value) | replaces the whole map |
> | drop one channel's role override | `--field-unset-key channel_roles.C0…` | replaces the whole map |
>
> Each edit is atomic server-side (a row-locked read-modify-write), so two
> operators appending different channels at the same time can't lose each
> other's write. Bare `-f`/`--field` still REPLACES — reach for it only when you
> genuinely mean to overwrite the whole value.

---

## 6. Verify

```sh
# bridge pod is up and chose Socket Mode
kubectl -n gardenio rollout status deploy/gardenio-slack-bridge --timeout=120s
kubectl -n gardenio logs deploy/gardenio-slack-bridge | grep -i 'socket mode'
#   expect: "Slack Socket Mode bot enabled"

# no new ingress object exists for the bridge (Socket Mode == no inbound)
kubectl -n gardenio get ingressroute 2>/dev/null | grep -i slack || echo "no slack ingress (correct)"
```

Then in Slack: **@mention the gardenio app in the invited channel** (or DM it).
You should get an agent card back and an agent spawned in project `gardenio`.

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Pod `CreateContainerConfigError` | Secret `gardenio-slack-bridge-creds` missing or wrong keys. Recreate per §2. |
| Logs show webhook mode, not Socket Mode | `app-token` (`xapp-…`) missing/empty in the secret. Socket Mode engages only when `SLACK_APP_TOKEN` is set (`gardenio/controller/cmd/slack-bridge/main.go:185`). |
| `@mention` does nothing | App not invited to the channel, or `message.channels`/`app_mention` event subs not enabled. Re-check §1. |
| DM does nothing | `im:history`/`im:read`/`im:write` scopes or `message.im` event sub missing. |
| Messages route to wrong/no project | Channel not in the project bead `slack_channels` (§5); DMs use `defaultDMProject`. |
| Helm fails on `edgeAuth`/HMAC | Pre-existing in-box install prerequisite, unrelated to this overlay — see `gardenio/docs/gardenio-in-a-box-install.md` §A.4. |
| `helm upgrade` hangs the full timeout, then rolls back | `--wait` is blocking on a crashlooping *sibling* deployment (e.g. `gardenio-beads-viewer`), not on the bridge/controller. Re-run without `--wait` and watch the rollout by hand — see §4 callout. |
| Upgrade fails: resource "exists and cannot be imported" / "not owned by the release" | A `slack-bridge` Deployment or PVC was created out-of-band (`kubectl apply`) and lacks Helm ownership metadata. Stamp the `meta.helm.sh/*` annotations + `managed-by=Helm` label, then re-upgrade — see §4 callout. |
| `@mention` reaches the bridge but no agent pod spawns | The bridge can be running while the **controller** (`gardenio-agents`) is off — `agents.enabled=false` means nothing spawns. The bridge alone is not enough; the helm reconcile must turn the controller on too. Verify: `kubectl -n gardenio get deploy gardenio-agents`. |

## Rollback

```sh
# drop the slack overlay from the helm command and re-upgrade, OR:
helm upgrade gardenio gardenio/helm/gardenio -n gardenio --reuse-values \
  --set slackBridge.enabled=false
kubectl -n gardenio delete secret gardenio-slack-bridge-creds   # optional
```
Socket Mode is outbound-only, so turning the bridge off removes the WebSocket;
nothing inbound was ever opened.
