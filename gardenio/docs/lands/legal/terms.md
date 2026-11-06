---
doc: terms
title: Gardenio Lands Terms of Service
version: 2026-10-19-draft.1
status: draft
effective: null
requires_reacceptance: true
---

# Gardenio Lands Terms of Service

> **Draft.** Not in force. Values in `{{BRACES}}` wait on the owner decision recorded in
> [README.md](README.md#owner-decision). Have a lawyer read the set before the first real
> tenant (step 11.3).

These terms are an agreement between you and {{ENTITY}} ("we", "us"), which runs Gardenio
Lands (the "Service"). By selecting **Accept** on `/lands/` you agree to these terms, the
[Acceptable Use Policy](aup.md) and the [Privacy Policy](privacy.md), at the version shown
beside each. If you do not agree, do not use the Service.

## 1. What the Service is

The Service gives an approved applicant a **land**: a private workspace tied to one GitHub
repository, where AI agents work on that repository at the request of the land's people.
A land comes with:

- a **steward**, an agent that turns requests into plans, asks the owner to approve them and
  starts other agents (**gardeners**) to carry them out;
- conversation channels, including `#general` and `#support`;
- a **purse** of prepaid credits that pays for the agents' work.

Agents push branches and open pull requests on your repository. **They do not merge.** You
decide what lands in your repository.

## 2. Who may use it

1. You need a GitHub account with a verified primary email address. Signing in with GitHub is
   the only way in.
2. You must be at least 18, or the age of majority where you live if that is higher, and able
   to enter a contract.
3. If you use the Service for an organisation, you confirm you may bind it to these terms, and
   "you" includes it.
4. One person, one account. Do not share your sign-in or let others act as you.

## 3. Applying for a land

1. You apply for a land (a "plot application") naming one repository you administer on
   github.com and installing the Gardenio Lands GitHub App on it.
2. We review every application by hand and may approve, deny or ask a question, for any
   reason or none. Approval is not guaranteed and does not continue indefinitely.
3. The person who applies is the land's **owner**. The owner may invite **members** and
   **guests** by GitHub login. The owner is responsible for the people they invite and for
   what agents do at their request.
4. {{PRIVATE_REPOS_CLAUSE}}

## 4. Credits and the purse

1. Credits are granted by us, by hand, into your land's purse. In this version of the Service
   you do not buy credits from us and we do not invoice you.
2. One credit is {{CREDIT_UNIT}} (owner question OQ1, decision gd-7sTt20714N).
3. Agents' AI model usage draws down the purse at the rate in 4.2. Spend is measured within minutes, not instantly, so the
   purse can go slightly below zero; we absorb that overrun.
4. When the purse reaches its limit, no new agent starts and running agents are asked to save
   their work and stop. Work resumes only if more credits are granted.
5. Credits have no cash value, cannot be transferred, sold or moved to another land, and end
   when the land closes.
6. We may correct a grant that was made in error, with a reason recorded in the land's credit
   ledger, which the owner can see.

## 5. Your content and your repository

1. **You keep what is yours.** Your repository, the messages you write, and the code, plans and
   other output agents produce for your land ("Output") belong to you as between you and us.
2. **You give us what we need to run the Service.** You grant us a worldwide, non-exclusive,
   royalty-free licence to host, copy, process, transmit and display your content and Output,
   and to send it to the providers named in the [Privacy Policy](privacy.md), only to provide,
   secure and support the Service. This licence ends when your data is deleted under the
   Privacy Policy.
3. **You have the right to point agents at it.** You confirm you have the rights needed for
   agents to read and change your repository, and that doing so does not break anyone else's
   rights or any agreement you are bound by.
4. **We can see your land.** The operator can see everything in every land, including agent
   transcripts and terminal screens, to run, secure and support the Service. Tenants never see
   an agent's terminal. Do not put into a land anything you are not willing for the operator to
   see.
5. **We do not train on your content.** We do not use your content or Output to train AI
   models, and we do not sell it.

## 6. AI output

Agents are AI systems. Their Output can be wrong, incomplete, insecure, or similar to code
written by others. You are responsible for reviewing Output before you merge, run, deploy or
rely on it, and for the licences and dependencies it brings into your repository. Agents run
the commands your repository's code and tools tell them to run; treat their pull requests as
you would a stranger's.

## 7. Acceptable use

You and everyone in your land must follow the [Acceptable Use Policy](aup.md). It is part of
these terms.

## 8. Availability and support

1. The Service is offered as is. Until an SLA document is published and named here, we make no
   promise of availability, response time or remedy.
2. Support is in your land's `#support` channel, or by mail to {{SUPPORT_EMAIL}} if you cannot
   sign in.
3. We may change, suspend or stop any part of the Service. For a change that removes something
   you rely on, we will tell land owners by email at least 14 days ahead where we reasonably
   can.

## 9. Freezing and closing a land

1. **We may freeze a land at once**, without notice, if we believe it breaks these terms or the
   AUP, puts the Service, other tenants or anyone else at risk, or if the law requires it. A
   frozen land runs no agents and accepts no writes; you can still read it and talk to us in
   `#support`.
2. **We may close a land** after a freeze that is not resolved, for a serious or repeated
   breach, if the Service stops, or with 30 days' notice for any other reason.
3. **You may close your land** at any time by asking in `#support`. You may also uninstall the
   GitHub App from your repository at any time, which stops agents from reaching it.
4. When a land closes, its agents stop, its members lose access, its credits end, and its data
   is deleted on the schedule in the [Privacy Policy](privacy.md#5-how-long-we-keep-it). Export
   anything you want to keep before you ask us to close it; your repository itself stays on
   GitHub and is not deleted.
5. Sections 5.1, 6, 10, 11, 12 and 14 survive the end of these terms.

## 10. Disclaimer

To the extent the law allows, the Service and all Output are provided **as is and as
available**, without warranties of any kind, express or implied, including merchantability,
fitness for a particular purpose, non-infringement, accuracy and uninterrupted or error-free
operation.

## 11. Limitation of liability

To the extent the law allows:

1. Neither party is liable to the other for indirect, incidental, special, consequential or
   punitive damages, or for lost profits, revenue, data or goodwill, however caused.
2. Our total liability arising out of or relating to these terms or the Service is limited to
   {{LIABILITY_CAP}}.
3. Nothing in these terms limits liability that cannot be limited by law, such as for fraud or
   for death or personal injury caused by negligence.

## 12. Indemnity

You will defend and indemnify us against third-party claims arising from your content, your
repository, your instructions to agents, or your breach of these terms or the AUP.

## 13. Changes to these terms

We publish every change as a new version with a new version id. When a change is material, we
email land owners and every user must accept the new version on their next visit before they
can keep using the Service. Earlier versions stay available on request and in the repository's
history.

## 14. Law and disputes

These terms are governed by the laws of {{JURISDICTION}}, without regard to its conflict-of-law
rules. Disputes go to the courts of {{VENUE}}, unless the law where you live gives you the
right to bring them elsewhere.

## 15. General

These terms, the AUP and the Privacy Policy are the whole agreement about the Service. If a
part is unenforceable, the rest stands. Not enforcing a right is not a waiver. You may not
assign these terms without our consent; we may assign them to a successor to the Service.

## 16. Contact

{{ENTITY}}, {{CONTACT_ADDRESS}}. Email: {{SUPPORT_EMAIL}}.
