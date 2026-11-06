# Autonomous Slack self-test for a gardenio-in-a-box — Operator + Gardener Runbook

> Tracking bead: [ka-0000005899](https://gardenio.e2e.example.com/beads/ka-0000005899) ·
> Epic: [ka-0000006506](https://gardenio.e2e.example.com/beads/ka-0000006506)
> (gasbox currency + availability) · Gardener: ka-0000002049 ·
> Script: [`scripts/inbox/slack-selftest.sh`](../../scripts/inbox/slack-selftest.sh)

This lets the gasbox gardener verify the **real Slack round-trip** — `@gardenio <text>`
→ bridge spawns an agent → agent reacts/replies — **with no operator in the loop**.
Today the gardener can only prove agents *can* schedule by SSHing to the box and
reading pod status with `kubectl`; it never exercises the Slack path a human uses.
This closes that gap.

## Why a Slack USER token (not the box's bot token)

The bridge **drops any mention authored by a bot** and ignores messages from its
own user id:

```go
// controller/internal/bridge/bot_mentions.go
func (b *Bot) handleAppMention(...) {
    if ev.BotID != "" { return }   // bot-authored mentions are dropped
    ...
}
// controller/internal/bridge/bot.go handleMessageEvent:
if ev.User == b.botUserID || ev.User == "" { return }
```

So the box's own `xoxb-…` bot token **cannot self-trigger**. Driving the workflow
autonomously requires a **second, human-style identity**: a Slack **user token
(`xoxp-…`)** for a dedicated user in the same workspace (the box currently runs in
the **botsberg** workspace, team `T0BB35ATDC0`, channel `C0BA2HJGLLB`).

The mention gate is currently **off** on the box (`MENTION_GATE_ENABLED` unset), so
any workspace user may mention the bot. If the gate is ever turned on
(`KEYCLOAK_MENTION_ROLE=gardenio-mention` is configured), the self-test user must
hold the `gardenio-mention` realm role (or the `mention` capability on its user
bead) or the mention is denied — see `checkMentionPermission` in `bot_mentions.go`.

## Operator step (once): mint the user token

In the **botsberg** Slack workspace:

1. <https://api.slack.com/apps> → **Create New App** → *From scratch*
   (name it e.g. `gasbox-selftest`).
2. **OAuth & Permissions → User Token Scopes** (NOT bot scopes):
   `chat:write`, `channels:history`, `channels:read`, `reactions:read`.
   For a **private** test channel add `groups:history`, `groups:read` instead of
   the `channels:*` pair.
3. **Install to Workspace** → authorize → copy the **User OAuth Token** (`xoxp-…`).
4. Make sure that user is a **member of the channel** the box posts to
   (`C0BA2HJGLLB`, or whichever `SLACK_CHANNEL` the box uses).

Hand the `xoxp-…` token to the gardener path via Secrets Manager (below). The token
**never goes in git** — same rule as the bot/app tokens (`AUDIT §J6`).

## Where the token lives

Stored in the **POC account (123456789012)** Secrets Manager — the same account the
gardener already reads `gardenio/customer-poc-aws-creds` from:

```sh
# POC creds (the gardener already exports these; see the epic's Access section)
aws secretsmanager create-secret \
  --name gardenio/gasbox-slack-selftest-user-token \
  --secret-string '{"SLACK_SELFTEST_TOKEN":"xoxp-…","SLACK_SELFTEST_CHANNEL":"C0BA2HJGLLB","SLACK_SELFTEST_BOT_USER":"U0PERSON023"}'
# (or `put-secret-value` to rotate an existing secret)
```

> The gardener reads SM via the POC AKIA pair (`gardenio/customer-poc-aws-creds`) it
> already uses for EC2 Instance Connect — no new IAM, no on-box change required. The
> self-test runs from the **gardener pod itself** (outbound HTTPS to slack.com); it
> does NOT need to be on the box, because the box's socket-mode connection is what
> carries the inbound event.

## Running it

```sh
# From the gardener pod (or anywhere with outbound HTTPS + the token):
SLACK_SELFTEST_TOKEN=xoxp-… \
  scripts/inbox/slack-selftest.sh --channel C0BA2HJGLLB --timeout 180 --cleanup
echo $?   # 0 = round-trip verified, 1 = posted but no bot reaction, 2 = usage/precondition
```

What it does:

1. `auth.test` — confirm the user token authenticates; capture the team.
2. Resolve the bot user id in the channel (or pass `--bot-user U0…`) so it posts a
   faithful `<@bot> …` mention.
3. `chat.postMessage` a `@gardenio ping — … <nonce>` line into the channel.
4. Poll up to `--timeout` seconds for **either** a lifecycle reaction on our message
   (`eyes` / `hourglass_flowing_sand` / `white_check_mark`) **or** a bot-authored
   threaded reply. Either is sufficient proof the bridge spawned.
5. `--cleanup` deletes the probe message afterward so the channel doesn't fill up.

Exit code is the verdict — the gardener keys its "agent-triggering" assertion on it.

## Gardener wiring (ka-0000002049)

The gardener's playbook step 2 (AGENT-TRIGGERING PROBE) gains a Slack arm: after the
cheap on-box pod check, fetch the token from SM and run `slack-selftest.sh`. A clean
exit 0 is the **operator's real bar** — a human-style mention actually spun up an
agent. The gardener reports (b) "real agent reached Running + replied to a Slack
mention this cycle" only when this passes; on exit 1 it does NOT claim green and
investigates (socket-mode dropped? agents disabled? pods Pending? mention-gate
denying the user?).

## Failure triage (exit 1)

The mention reached Slack but no agent reacted. In order of likelihood:

- **socket-mode disconnected** — `kubectl logs deploy/gardenio-slack-bridge | grep -i "Socket Mode"`; should show `connected`.
- **agents disabled** — `agents.enabled` must be true in the live values.
- **agent pods Pending** — the 8-vCPU box fits ~one agent; check the LimitRange /
  per-project cpu_request (bug ka-0000006038). A stuck pod = no reply.
- **mention-gate denying** — if `MENTION_GATE_ENABLED=true`, the self-test user needs
  the `gardenio-mention` role; grep the bridge logs for `mention_audit: denied`.
- **wrong channel / user not a member** — the token's user must be in the channel.
