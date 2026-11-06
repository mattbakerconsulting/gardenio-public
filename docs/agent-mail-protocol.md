# Agent mail protocol

An agent's inbox is its mail. Anything the fleet needs to push to an agent
(a Slack mention, a decision answer, a forge review, a kill, an approved
escalation) is a `type=mail` bead assigned to the agent. The agent's own
process subscribes to its mail and delivers it to its terminal. Nothing
dials an agent.

Design: `docs/superpowers/specs/2026-10-11-local-agents-escalations-design.md`
§3 (mail is the inbox), §8.1 (`control:escalation-approved`), §10 (error
handling). The label grammar is implemented once, in
`gardenio/controller/internal/mailproto`; Go code in the controller module
uses its constants and helpers rather than string literals.

## Topic

There is one agent-inbound topic: **`beads.bead.created`**, filtered to
`type=mail`. The agent subscribes with the daemon's `agent` scope:

```
GET /v1/events/stream?topics=beads.bead.created&agent=<agent bead id>
```

The `agent` scope restricts the stream to events on that agent bead and on
beads whose `assignee` is the agent's name. Nudges, decision answers, review
comments, kills and approved escalations are not separate topics; they are
all mail on this one topic, told apart by labels.

## Schema

The event payload is `{"bead": <bead>}`. A mail bead is an ordinary bead:

| Field | Meaning |
|---|---|
| `type` | `mail` |
| `kind` | `data` |
| `assignee` | the recipient agent's name |
| `title` | the subject, one line |
| `description` | the body |
| `priority` | 0 critical, 1 high, 2 normal (default), 3 low |
| `status` | `open` = not yet read; closed by `gb mail read` (or by the agent's subscriber for `control:kill` and `control:permission`) |
| `labels` | the three label families below |

No new bead type; `control:permission` mail alone carries fields
(`request_id`, `behavior`). Delivery state is the mail's status,
ordering is creation order.

The stream and the subscriber's drain still apply the reader's project
scope, and an agent reads with a project-scoped credential (its per-agent
token, or the runner bearer), so mail must carry its recipient's
`project:<name>` label. The daemon stamps it at create on mail that has
none: the project of the agent bead whose `fields.agent` is the assignee,
or of the runner bead for `runner:<id>` mail, when that bead has exactly
one (gd-D8jkm7eyEO). Senders need not add it.

## Labels

| Family | Form | Meaning |
|---|---|---|
| Sender | `from:<sender>` | Who sent it: an agent name, `slack-bridge`, `github-bridge`, `gitlab-bridge`, the daemon actor. Missing means `unknown`. |
| Delivery | `delivery:interrupt` | Deliver now, with the body inline. Priority 0 or 1 is treated the same way. |
| Control | `control:<verb>` | Not text to read: an instruction to the agent's subscriber. Closed set of verbs, below. |

### Control verbs

| Verb | Sent by | The agent's subscriber |
|---|---|---|
| `kill` | the bridge (kill from Slack), the daemon on runner revoke, an operator | closes the mail with `control_acted=<RFC 3339>` in the close body, asks coop to shut down gracefully, exits |
| `escalation-approved` | the daemon, from inside the escalation approve handlers after the execution claim is taken (spec §8.1; not emitted yet) | passes the mail to the subscriber's `OnControl` hook (the escalation executor); the body names the escalation |
| `permission` | the Slack bridge, on an Allow/Deny click (or a Commons answer) for a Claude Code tool-approval prompt; never a runner or an agent token | POSTs the mail's `request_id` and `behavior` (`allow` or `deny`) fields to the loopback gardenio-channel server's `/permission` (port `GARDENIO_CHANNEL_PORT`, default 8789, bearer `GARDENIO_CHANNEL_TOKEN` or `COOP_BROKER_TOKEN`), which answers the prompt; then closes the mail with `control_acted`, delivered or not, since a verdict answers one session's prompt (`cmd/gb/mail_permission.go`) |

`gb mail send --label control:<verb>` refuses any verb not in this table, so a
typo cannot create a control mail no subscriber understands. Who may set
`control:` at all is the daemon's rule (spec §3.3, task gd-FNpCYoB8zo): the
bridge, the controller, an operator, or the runner's owner, never one agent
against another. `control:permission` is narrower still: the runner is
refused it even for its own agent, since its credential shares the agent's
machine and would let a local agent approve its own prompts.

The same identities, and only they, may end control mail early. The daemon
refuses anyone else (in practice the recipient's own token) a delete, a
status change through update, or a close of control mail, so an agent cannot
cancel a kill before its subscriber honours it (gd-NyJHOYuapZ). The one
exception is a `POST /v1/beads/{id}/close` whose body stamps `control_acted`:
the subscriber closes kill mail that way, and it holds the same token as the
agent's own session, so the daemon cannot tell the two apart. A session that
forges the stamp leaves a closed kill mail saying it was acted on, with the
agent still running.

## Delivery

The subscriber runs beside coop inside `gb agent start` (pods and `--local`
alike, `internal/mailsub`):

- **Ordinary mail** is delivered as the one-line pointer
  `New mail from <sender>: <title> — run 'kd show <id>' to read`.
- **Interrupt mail** (`delivery:interrupt`) is delivered with its body
  inline: `Mail from <sender> [<id>]: <title>` then the body.
- **Control mail** is handled, not shown, per the verb table.
- **Retries:** three attempts two seconds apart; on failure the mail stays
  open and is retried on the next drain.
- **Reconnect:** exponential backoff from 1 s capped at 60 s, resuming with
  `Last-Event-ID`; every connect first drains open mail, oldest first, so mail
  that fell out of the daemon's replay ring is still delivered. Delivered ids
  are remembered per process, so replay plus drain never delivers twice.

## Sending

```bash
gb mail send <agent> -s "subject" -b "body"                 # ordinary
gb mail send <agent> -s "subject" -b @file --interrupt      # deliver now
gb mail send <agent> -s "Stop" -b "<reason>" --label control:kill
```

`--label` is repeatable and appends after `from:<sender>` (and
`delivery:interrupt`, if set).
