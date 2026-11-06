# Lands support runbook

Track B step 11.1 (gd-UBeJ3kVicQ, spec §6.1). How a tenant asks for help, what that becomes,
and what the operator does with it, from the first message to a closed request. Outages and
security events also follow the [incident process](incident-process.md); this page covers the
single request.

**These are aims, not promises.** There is no SLA for Open day (owner decision gd-yjw85aEUao,
`no-sla-v1`). The Terms §8.1 promise no response time, and the times below are the operator's
own targets. If the payments decision (step 11.6) publishes an SLA, its response times replace
the table in [Response targets](#response-targets) and become promises.

One person is the operator, the support desk and the incident responder (spec §9). Everything
here is sized for that.

## Where requests come from

| From | How | What the operator gets |
|---|---|---|
| A land member, in the Commons | A post in their land's `#support` | A **support bead** in the land and a **Slack DM** ([below](#what-a-post-in-support-becomes)) |
| The land's steward | A post in `#support`: its only way to the operator (spec §4.2), e.g. "the purse is empty" | The same |
| Someone who cannot sign in | Mail to `support@lands.clonk.tv` | The mail, forwarded to the operator's mailbox ([below](#mail-to-support)) |

A tenant cannot see the operator's land, so there is no shared support channel: each tenant land
has its own `#support`, seeded by the provisioner with the land's other channels. Its members are
the land's members; the operator, an admin, sees every land.

## What a post in #support becomes

The slack-bridge watches every new message (`internal/bridge/lands_support.go`):

- **A top-level post** files one **support bead** for its thread: a `task` in the land's project,
  labelled `support`, assigned to the support operator, titled `Support (<land>): <first line>`.
  The members see it in their land. The description quotes the post; `support_message_id` and
  `support_channel_id` point back to it.
- **A reply in that thread** is added to the bead as a comment.
- **The operator is DMed** in Slack for each, with the land, the author, a link to the bead and up
  to 500 characters of the post. It works like an escalation DM. The bead is stamped
  `support_notified_at` and `support_notified_via=dm`.
- **When no DM can be sent** (no operator configured, no Slack id for them, or Slack fails), a
  pointer goes to the fallback channel instead (`support_notified_via=channel`). It holds the land
  and the bead id, and **never the tenant's words**: a tenant's conversation does not reach the
  fleet's Slack channels (B-D11). Only the operator's own DM carries the text, as the Privacy
  Policy §4 says.
- **Nothing is lost while the bridge is down.** Every 5 minutes a sweep re-reads the last day's
  top-level `#support` posts in every tenant land, and tells the operator about any whose bead is
  not stamped. Replies are not replayed; one missed while the bridge was down is still in the
  thread.
- **Not requests:** the operator's own posts and the Commons' notices and cards.

Setup: `slackBridge.landsSupport.operator` (the operator's email; it must resolve to a Slack id
the way escalation DMs do) and, optionally, `slackBridge.landsSupport.pointerChannel`, which
defaults to the bridge's `SLACK_CHANNEL`. With no operator set, beads are still filed, unassigned,
and every request is a pointer.

## Response targets

| Kind | Means | First answer, aim |
|---|---|---|
| **Urgent** | The land cannot work at all (no agent can run, members cannot sign in or post), a security or data concern, or money: credits spent wrongly | Within **4 hours** while the operator is working, else first thing the next working day. A security or isolation concern is an incident: [incident-process.md](incident-process.md), Sev 1 |
| **Normal** | Everything else: a stuck agent, a question, a feature ask, more credits | Within **2 working days** |
| **Privacy request** | A copy of the data, a correction, deletion, or an objection (Privacy Policy §6) | Acknowledge as Normal; complete within **30 days**, the one time the Policy does promise |

These are the response values recommended to the owner for an SLA (incident-process.md, "If an
SLA is published later"). The operator's support hours are not stated anywhere yet; tenants are
told none. A "first answer" is a human reply in the thread, even "seen, looking into it". It is
not the fix.

## Triage

Open the bead from the DM. Read the thread in the land's `#support` in the Commons
(`/garden?land=<land>`, `#support`), then:

| The request is about | Do |
|---|---|
| **Credits**: the purse is empty, or more are wanted | `gb lands purse <land>`. A grant is the owner's call; grant with `gb lands credit add <land> <credits> --reason "support <bead>: …"`. When the purse is empty the steward has parked work (`purse_empty`) and wakes on the grant |
| **Spend that looks wrong** | `gb lands purse <land>` and `gb lands credit list <land>`. If we caused it, [refund](#refunding-credits) |
| **Signing in, or a teammate who cannot join** | Their GitHub account must be the one the invite names (step 8.9). Check the invite in the land, and the user in Keycloak (`keycloak-provisioner.md`) |
| **An agent that is stuck, failing or doing the wrong thing** | Look at the agent in the land. The tenant's own code or instructions: answer in the thread. Gardenio misbehaving: [it is our bug](#when-it-is-our-bug) |
| **A merged PR or the repository** | The tenant merges on GitHub, and agents reach the repo only through the Lands App (step 7.6). If they uninstalled it, agents cannot push |
| **Something Lands cannot do** | Say so plainly. Note it on the bead with the `feature-ask` label, for the Open-day retrospective (spec §6.2 step 5) |
| **Closing the land** | Terms §9.3: the owner may ask at any time. Confirm it is the owner, remind them to export first (Terms §9.4), then `gb lands close <land>` |
| **A privacy request** | Privacy Policy §6. Confirm the requester is the person or the land's owner. Record what was done on the bead |
| **Abuse, a leak, one land reaching another** | Stop. It is an incident: [incident-process.md](incident-process.md), Sev 1. [Freeze](#when-to-freeze) first if it is still going on |

Answer **in the thread** in the Commons. The operator can post in any land. Set the bead's
priority to match: `kd update <bead> --priority 0` for urgent. A request that turns into work in
the tenant's land, such as "please re-run this", is the steward's job: say so in the thread, and
the steward turns the member's ask into a plan.

### When it is our bug

The support bead stays in the tenant's land: it is the tenant's record, and they can see it. A
fault in gardenio itself gets **its own** bug in `project:gardenio`, which tenants cannot see:

```bash
kd create "Lands: <what broke, in our words>" --type=bug -l project:gardenio -l lands \
  -d "Reported in support bead <bead> (land <land>). <what happened, what we found>."
```

- Copy only what the fix needs from the tenant's land into the gardenio bug: no repo contents, no
  secrets, no member names unless they matter. The Privacy Policy lets the operator see a land's
  data to support it; it does not make that data fleet-wide.
- Never add a second `project:` label to the support bead to "share" it. A bead with two project
  labels is visible in both lands.
- Comment the gardenio bug's id on the support bead, so the operator can find it again. The
  tenant sees the id but cannot open it. Tell them in the thread, in words, what is being fixed.

## When to freeze

`gb lands freeze <land> --reason "…"` is for **harm in progress**, not for a disagreement:

- the land is attacking something (AUP §2), abusing the Service (AUP §3), sending spam (AUP §4),
  or doing something illegal (AUP §5);
- its agents are spending credits in a loop the steward is not stopping, and the owner has not
  answered;
- an isolation or security doubt about the land (incident-process.md, step 3).

Then follow AUP §6: tell the owner what was seen and ask for an explanation, in `#support` **and
by email**. A freeze is a safety step, not a finding. `gb lands thaw <land>` when it is settled.

**A frozen land's `#support` stays open** (Terms §9.1, gd-mJw9pxK2wu). It is the one place the
land's members can still post: says and replies there are taken from anyone in the land whose role
lets them post, whether or not they had joined `#support`, since nobody can join a channel during a
freeze. Everything else stays refused, including the land's agents, other channels, cards, joins
and reactions. The Commons shows them the composer under a banner saying so. Their posts become
support beads and DMs as usual, so the owner's answer to the AUP §6 question arrives like any other
request.

## Refunding credits

Credits we caused to be spent are given back as a grant:

- an agent burned credits because of a gardenio bug, or in an incident;
- a grant was made in error (a negative grant corrects it: Terms §4.6).

```bash
gb lands credit add <land> <credits> --reason "refund, support <bead>: <why>; gardenio bug gd-…"
gb lands credit add <land> -<credits> --reason "correction: duplicate grant <grant-bead>"
```

Work out the amount from `gb lands purse <land>` and the agent's spend, and round in the tenant's
favour. Say in the thread what was refunded and why. Credits have no cash value (Terms §4.5), so a
refund is always credits. The tenant's own code or instructions spending credits is not refunded.

## Mail to support@

Mail to `support@lands.clonk.tv` is received by SES and forwarded to the operator's mailbox
(`gardenio/terraform/modules/ses-support-inbound`):

- It arrives as `[support] <subject>` from `<sender> via support <support@lands.clonk.tv>`.
  Replying goes to the sender (Reply-To). **The reply is sent from the operator's own mailbox**,
  so it shows that address.
- `[spam?]` in the subject means SES's spam check failed. It is forwarded anyway, because a real
  tenant writing from a new address is often scored badly.
- Check `X-Gardenio-Support-Verdicts` before acting on what a mail asks for. A failing `spf`,
  `dkim` or `dmarc` means the From may be forged.
- A `[support] not forwarded: …` notice means the mail failed the virus scan or could not be
  re-sent. The raw mail is in `s3://gardenio-support-mail-<account>/support/<id>` for 30 days.
  Open a virus-flagged one only in a sandbox, if at all.
- **Email is not proof of who someone is.** Do nothing that changes a land on the strength of a
  mail alone: close it, delete data, add a member, or grant credits. Ask the person to confirm from
  the GitHub account linked to the land, or from inside the land once they can sign in.
- When the mail is about a land and the sender is a member, file the support bead by hand, so the
  request has a record in the land:

  ```bash
  kd create "Support (<land>): <subject>" --type=task -l support -l project:<land> \
    --assignee <operator email> -d "By mail to support@ from <sender>, <date>. <summary>."
  ```

  Otherwise keep it in the mailbox.
- If forwarding fails, the alarm `gardenio-support-forward-errors` emails the edge-probe address.
  The mail is still in the bucket.

## Daily and at the end of Open day

Every working day, check the open requests across all lands:

```bash
kd list -l support --status open --all-projects
```

An open support bead with no human reply in its thread is late once it is past its
[target](#response-targets). Close a bead when its request is answered:
`kd close <bead> --reason "<what was done>"`. The thread stays; a member can reopen the matter by
posting again, which files a new bead.

For the Open-day retrospective (spec §6.2 step 5), the support load is these beads:

```bash
kd list -l support --all-projects -s open -s closed --json
```

It gives the count per land, the first-answer times (from the bead and its thread), what was
urgent, what was our bug, and what was a `feature-ask`.
