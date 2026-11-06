---
doc: aup
title: Gardenio Lands Acceptable Use Policy
version: 2026-10-19-draft.1
status: draft
effective: null
requires_reacceptance: true
---

# Gardenio Lands Acceptable Use Policy

> **Draft.** Not in force. Part of the [Terms of Service](terms.md); see
> [README.md](README.md) for how the set is versioned.

This policy says what you may not use Gardenio Lands for, and what happens if you do. It
applies to everyone in a land (owner, members and guests) and to everything the land's agents
do at their request. "Pointing an agent at" something includes asking for it in a message, a
plan, an issue, a file in the repository, or any other way an agent reads instructions.

## 1. Use your own repository, for your own work

- Agents work on the one repository your land was approved for. Do not use them to reach,
  change or copy other repositories or accounts you do not control.
- Do not put code, data or secrets into a land that you are not allowed to share with us and
  with the AI providers named in the [Privacy Policy](privacy.md).

## 2. Do not point agents at other people's systems

You may not point an agent, or code it runs, at any system, network, account or person that
you do not own or have written permission to test. In particular, no:

- scanning, probing, fuzzing or vulnerability testing of third-party hosts;
- exploiting vulnerabilities, or building or deploying malware, ransomware or botnets;
- credential stuffing, password spraying, brute-forcing logins, or using stolen or leaked
  credentials, tokens or session cookies;
- denial-of-service traffic, load testing of systems you do not own, or anything that
  degrades someone else's service;
- scraping that ignores a site's terms, `robots.txt` or rate limits, or that collects personal
  data about people without a lawful basis.

Security work on **your own** repository and on systems you own is allowed. Keep it inside your
own systems.

## 3. Do not abuse the Service itself

No:

- cryptocurrency mining, proof-of-work, or other compute use unrelated to developing your
  repository;
- attempts to reach another land, the operator's systems, the Kubernetes API, cloud metadata
  endpoints, or any internal address, or to get around a land's network, resource or spend
  limits;
- attempts to make an agent reveal its credentials, the system prompt of another land, or
  anything outside your land, including through prompt injection planted in your repository;
- running a proxy, VPN, file host, or public service from an agent's workspace;
- creating many accounts or applications to get more credits or to evade a freeze;
- reselling or sublicensing access to your land without our written permission.

If you find a way through a limit by accident, stop and tell us in `#support`. Reporting a
security problem in good faith is welcome and is not a breach of this policy.

## 4. Do not send spam or harass

No unsolicited bulk messages of any kind (email, SMS, chat, social posts, issues or pull
requests on others' repositories), no automated account creation on other services, and no
harassment, threats or abuse of anyone, including other people in your land and the operator.

## 5. Nothing illegal or harmful

No use that breaks the law where you are, where we are, or where the affected people are,
including:

- content that sexualises minors, or non-consensual intimate content;
- infringing copyright, trademarks or other rights, or removing licence notices you are bound
  to keep;
- processing personal data without a lawful basis, or special-category data (health, biometric
  and the like) unless you have told us first in `#support` and we have agreed;
- fraud, phishing, impersonation, or deceptive content presented as real;
- evading sanctions or export controls;
- material that incites violence, or help with weapons capable of mass harm.

You must also follow the usage policies of the AI providers named in the
[Privacy Policy](privacy.md), and GitHub's terms.

## 6. What happens on abuse

1. **Freeze.** When we see or are told about a likely breach, we freeze the land first: no
   agent runs, nothing new is written, and running agents are stopped. We may revoke the land's
   GitHub App access and AI-provider key at the same time. A freeze is a safety step, not a
   finding.
2. **Ask.** We tell the owner in `#support` and by email what we saw, and ask for an
   explanation. We may keep the land frozen while we look.
3. **Close.** For a serious breach, a repeated one, or no answer within 14 days, we close the
   land. Its remaining credits end. We may refuse future applications from the same people or
   repositories.
4. **Report.** Where the law requires it, or where it is needed to protect others, we report
   the activity and the data needed to understand it to the affected party, GitHub, the AI
   provider, or the authorities. We keep the records of an abuse case for as long as the
   [Privacy Policy](privacy.md#5-how-long-we-keep-it) allows for that purpose.

We may skip steps 1 to 3 and close at once when there is serious harm or legal risk.

## 7. Reporting abuse

To report a land that breaks this policy, write to {{ABUSE_EMAIL}} with what you saw and when.
