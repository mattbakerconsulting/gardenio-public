---
doc: sla
title: Gardenio Lands Service Level Agreement
version: 2026-10-21-draft.1
status: deferred
effective: null
---

# Gardenio Lands Service Level Agreement

> **Deferred: there is no SLA for Open day.** The owner decided on 2026-10-21 (decision
> gd-yjw85aEUao, option `no-sla-v1`) not to promise availability, response times or a remedy
> for v1. The [Terms of Service](legal/terms.md) §8.1 keeps "no promise", and this draft stays
> unpublished, with its `{{BRACES}}` unfilled. The status page and the
> [incident process](incident-process.md) run anyway. The SLA comes back with the payments
> decision after Open day (Track B step 11.6, gd-cjItlf1QKP). The values recommended for it are
> in [incident-process.md](incident-process.md#if-an-sla-is-published-later).

This agreement is part of the Terms of Service. Words defined there (the Service, land, land
owner, member, credit, purse) mean the same here.

## 1. What we promise

1. **Availability.** In each calendar month (UTC), the Service is available at least
   **{{SLA_TARGET}}** of the time.
2. **Status.** We keep a public status page at {{STATUS_PAGE_URL}}. It is served from outside
   the Service, so it stays up when the Service is down. During an incident that stops your
   land working, we post a notice there and update it until the incident is over (section 5).
3. **Support response.** We answer support requests within the times in section 6.

## 2. How availability is measured

1. A prober outside the Service checks three things once a minute, from the public internet:
   the Lands front door (`/lands/`), the garden (`/garden`) and the Commons API. The status page
   shows each check's result and history.
2. A check **fails** when it gets no answer, or the wrong answer, within 10 seconds.
3. Each check's availability for a month is its passing checks divided by all its checks in
   that month, leaving out the hours in section 3. **The Service's availability for the month
   is the lowest of the three.**
4. An hour in which the prober itself did not run counts neither for nor against us.
5. These numbers are the only measure this agreement uses. We work them out from the prober's
   records, and on request we send you the figures for any month in the last 90 days.

## 3. What is not counted

Time when the Service is unavailable because of any of these is left out of the measure:

1. **Announced maintenance.** Maintenance posted on the status page at least
   {{MAINTENANCE_NOTICE}} before it starts, in whole hours, up to {{MAINTENANCE_CAP}} in a
   month. Maintenance beyond that counts against us.
2. **Your land's own state.** Your land frozen under the Acceptable Use Policy or the Terms,
   your purse at its wall, your land closed, or a pause you asked for.
3. **Things outside the Service.** Outages of GitHub, of the AI model providers, or of your
   repository, your network or your devices. The Service can be up while an agent cannot work
   because GitHub or a model provider is down; that time is not counted against us, but we
   still post it on the status page.
4. **Your code and your instructions.** Anything your repository makes the agents run, and any
   failure that comes from it.
5. **Beta features.** Anything the Service marks as beta or preview.
6. **Events beyond our reasonable control**, such as a failure of our cloud provider's whole
   region, a natural disaster, or an attack on the internet at large rather than on the
   Service. An attack on the Service itself counts against us.

## 4. The remedy: SLA credits

1. If the Service's availability in a month is below {{SLA_TARGET}}, each tenant land that
   was open in that month gets credits into its purse:

   {{REMEDY_TABLE}}

2. **We pay without being asked.** Within the first {{REMEDY_PAY_DAYS}} days of each month we
   work out the previous month's figure and grant the credits to every land owed them. The grant
   shows in your land's credit ledger with a reason that names the month and the incidents.
3. **You may claim.** If you think we owe you credits we did not pay, ask in your land's
   `#support` channel, or by mail to {{SUPPORT_EMAIL}}, within 30 days after the end of the
   month. We answer with the month's figures.
4. SLA credits are ordinary credits: they follow the Terms §4 (no cash value, they end when the
   land closes).
5. **SLA credits are your only remedy** for the Service being unavailable, to the extent the law
   allows. They do not change the limits on liability in the Terms §11.

## 5. Incidents

1. **We tell you.** When an incident stops tenant lands working, we post a notice on the status
   page within {{NOTICE_WITHIN}} of knowing about it, and update it at least every
   {{NOTICE_UPDATE_EVERY}} while it lasts. If the Commons is up, we also post in each affected
   land's `#support`.
2. **We tell you what happened.** After an incident that broke the availability promise, or
   that affected your data, we post a short account of the cause and what we changed in each
   affected land's `#support` within {{REVIEW_WITHIN}}.
3. **Security incidents** that affect your data follow the Privacy Policy as well: we tell you
   without undue delay and as the law requires.

## 6. Support response times

We answer, meaning a person replies to you (it may not yet be a fix), within:

{{RESPONSE_TARGETS}}

Support hours are {{SUPPORT_HOURS}}. Report anything urgent in `#support`; mail to
{{SUPPORT_EMAIL}} is for when you cannot sign in.

## 7. Changes

We may change this agreement as the Terms §13 describes. A change that lowers a promise takes
effect at the start of a calendar month, at least 30 days after we tell land owners by email.
