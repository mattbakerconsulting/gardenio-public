# Group DMs (Slack mpim)

Runbook for gardenio participation in Slack group DMs (mpim — multi-person IMs).
Design + phasing live on epic [`ka-0000003709`](https://github.com/mattbakerconsulting/gardenio).
The bot became a full mpim participant once Phase 0 shipped the manifest scopes
(`mpim:{history,read,write}` + `message.mpim`) and the Slack app was re-installed.

## Participate (respond in a group DM the bot is already in)

In a group DM, gardenio responds **only when explicitly @mentioned**. This is
deliberate: an mpim has several human members, so — unlike a 1:1 DM where every
message is implicitly for the bot — gardenio must be addressed by name or it stays
silent. Top-level chatter without `<@bot>` falls through to a no-op (no spam).

The **first** @mention in a group DM spawns an agent bound to that channel;
subsequent turns in the same conversation route to that agent. Because a group DM
has no thread anchor, the binding is channel-wide (`slack_thread_channel` = the
mpim id, empty `slack_thread_ts`).

**Rule of thumb:** always `<@gardenio>` the bot in a group DM, every turn you want it
to act. It will not infer that an un-addressed message is meant for it.

## Start (open a new group DM, optionally with a bound agent)

    gb slack group open U0123ABCD alice@x.com --project gardenio --role crew

Opens an mpim with the given members (Slack user IDs `U…`/`W…` or emails, resolved
bridge-side) and posts an intro. Passing `--project`, `--role`, or `--prompt`
(or `--spawn`) spawns an agent bound to the new channel; role defaults to `crew`.
Backs `POST /api/slack/groups/open`.

## Invite (add someone to a bot-started group DM)

    gb slack group invite <channel> U0999NEW carol@x.com

**Slack has no in-place mpim invite.** `conversations.invite` is channels-only, and
`conversations.open` with a different member set yields a *distinct* conversation
(new channel id, no shared history) — it never mutates the existing mpim. So
`invite` re-opens a **superset** mpim (existing members + the new user), migrates
the bound agent to the new channel (`SetThreadAgent(new)` + updates the agent
bead's `slack_thread_channel`), and posts a one-line context hand-off in **both**
the old and new group DMs. Expect the printed channel id to **differ** from the
input when the invitee was genuinely new. Inviting an existing member is an
idempotent no-op that returns the same channel. Backs
`POST /api/slack/groups/{channel}/invite`.

## Project / role selection

Selection works two ways, and they compose:

- **At start:** `--project <p>` / `--role <r>` on `gb slack group open`.
- **In an @mention:** lead the message with `project:<p>` and/or `role:<r>` (also
  `--project <p>` / `--project=<p>`, likewise for role) — e.g.
  `<@gardenio> project:gardenio role:crew look at the failing pipeline`. The override
  is stripped from the prompt before the agent sees it. Role defaults to `crew`
  when unset.

## See also

- `gb slack group --help`, `gb slack group open --help`, `gb slack group invite --help`
- E2E smoke: `gardenio/tests/e2e/scripts/test-group-dm.sh`
- Inbound routing: `handleMessageEvent` mpim branch in `controller/internal/bridge/bot.go`
