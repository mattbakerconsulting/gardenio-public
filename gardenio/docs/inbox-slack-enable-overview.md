# Slack-enable gardenio-in-a-box — findings, target, and plan

**Ask (Matt Baker):** interact with the gardenio-in-a-box setup via Slack; find
the prior EC2 k3s work, confirm access to an EC2 instance, make a plan, bundle it.

## What the prior EC2 k3s work is

The EC2 k3s body of work is **"gardenio-in-a-box" + its air-gap capstone "Tarboat"**:
a single EC2 box running on-box **k3s** with the full gardenio stack (controller,
beads, coopmux, slack-bridge, in-box LiteLLM), installed public-safe via
`scripts/bootstrap-ec2.sh` / `scripts/deploy-byo.sh` with two layered values
overlays. Key beads:

- **ka-0000005158** — from-scratch proof run: provision EC2 + drive `tarboat-validate.sh`.
- **ka-0000007708** — provisioned a fresh EC2 + staged inference cred (closed).
- **ka-0000004567** — Tarboat T3 release-grade from-scratch proof run.
- **ka-0000006148 / ka-0000006488** — proof a real agent pod did live inference on the box's k3s.
- Install runbook: `gardenio/docs/gardenio-in-a-box-install.md`; overlays in `deploy-values/`.

## Access — confirmed

- In-cluster fleet AWS role is in the **mgmt account (234567890123)**; the boxes
  live in the **AWS-poc account (123456789012)** and are driven by an operator
  over **SSH** (the in-cluster pod is intentionally not given the box SSH key).
- Two boxes are reachable right now (probed from the fleet):

  | Box | EIP | State |
  |---|---|---|
  | **gardenio-in-a-box** (recommended) | `192.0.2.91` | live full stack, app `2026.160.13`, on-box k3s, 443 up |
  | tarboat-proof (blank) | `3.224.147.106` | blank AL2023 scratch box, 22 up |

  (A third earlier box `3.222.176.151` no longer answers SSH.)

## Why Slack-on-a-box is cheap: Socket Mode

The slack-bridge supports **Socket Mode** — an *outbound* WebSocket to Slack.
Set `SLACK_APP_TOKEN` (`xapp-…`) and it auto-engages
(`gardenio/controller/cmd/slack-bridge/main.go:185`). **No public webhook, no new
ingress, no inbound 443 from Slack.** The in-box overlays ship
`slackBridge.enabled=false` (air-gap default); we flip it on with creds.

## Plan (3 operator inputs, then one helm upgrade)

1. **Operator (off-box, once):** create a Slack app — bot token `xoxb-…`, app
   token `xapp-…` (Socket Mode, `connections:write`), signing secret (optional),
   the scopes + event subs listed in the runbook; invite it to a channel; grab
   the `C0…` id.
2. **On the box:** `kubectl create secret gardenio-slack-bridge-creds` from those
   tokens (never in git).
3. **Set channel id** in `deploy-values/gardenio-inbox-slack.yaml`.
4. **`helm upgrade`** with the existing two in-box overlays **+ this new overlay
   layered last**; map channel→project on the project bead; verify the pod logs
   `Slack Socket Mode bot enabled`.

Full step-by-step: **`gardenio/docs/inbox-slack-enable-runbook.md`**.
Overlay (helm-template + helm-lint validated): **`deploy-values/gardenio-inbox-slack.yaml`**.

## Net effect

@mention or DM the gardenio app → spawns/answers agents on the box; bead
lifecycle + decisions post back to Slack; the whole box is driven from Slack
with no new open ports.
