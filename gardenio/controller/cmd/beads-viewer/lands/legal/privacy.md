---
doc: privacy
title: Gardenio Lands Privacy Policy
version: 2026-10-19-draft.1
status: draft
effective: null
requires_reacceptance: true
---

# Gardenio Lands Privacy Policy

> **Draft.** Not in force. Values in `{{BRACES}}` wait on the owner decision in
> [README.md](README.md#owner-decision), and every row in §5 must be true in the running system
> before this is published ([README.md](README.md#before-publishing)).

This policy explains what {{ENTITY}} ("we", "us") collects when you use Gardenio Lands, why,
who can see it, and how long we keep it. It is part of the [Terms of Service](terms.md).

## 1. The short version

- You sign in with GitHub. We keep your GitHub id, login and verified email.
- Your land holds your repository's code (while agents work on it), your conversations, the
  plans and tasks agents make, and records of what each agent did, including its transcript
  and screen.
- **The operator can see all of it**, to run, secure and support the Service. People in other
  lands see none of it.
- Agents send your code and messages to an AI model provider (Anthropic) to do their work.
- We do not sell your data, show you ads, or train AI models on it.
- When your land closes, we delete its content after {{RETENTION_DAYS}} days; backups age out
  within a further 35 days.

## 2. What we collect

| What | Where it comes from |
|---|---|
| **GitHub identity:** numeric id, login, verified primary email, display name | GitHub, when you sign in |
| **Account signals:** your GitHub account's age, public repository count and followers | GitHub, when you apply |
| **Your application:** land name, repository, whether it is private, purpose, credits requested, GitHub App installation id | You and GitHub, when you apply |
| **Agreement records:** which version of these documents you accepted, and when | You, when you select Accept |
| **Membership:** who owns your land, who is a member or guest, invites sent | You and your land's owner |
| **Repository content:** your code, history, issues and pull requests that agents read or write | GitHub, through the Gardenio Lands GitHub App on your one repository |
| **Conversation and work records:** messages in your land's channels, plans, tasks, decisions, comments, agent status | You, your land's people and its agents |
| **Agent transcripts:** each agent's full session with the AI model, including the model's reasoning text, the commands it ran and their output | The agents |
| **Terminal recordings:** snapshots of each agent's terminal screen as it works; values that look like credentials are masked where we can detect them | The agents |
| **Usage and credit records:** credit grants, spend per agent and per AI request (model, tokens, cost, time) | The Service and our AI gateway |
| **Service logs and metrics:** request logs, errors, IP addresses, timings, counts | Our servers |
| **Email:** messages we send you about your application and land, and anything you send to {{SUPPORT_EMAIL}} | The Service and you |

We do not ask for a password, payment details, phone number or postal address.

Do not put other people's personal data into a land unless you are allowed to share it with us
and our providers (see the [AUP](aup.md#5-nothing-illegal-or-harmful)).

**Private repositories.** If your land's repository is private, its contents, including code
you treat as confidential, appear in agent transcripts and terminal recordings, which the
operator can see. We treat them like the rest of your land: we use them only to run, secure and
support the Service, and we delete them on the schedule in §5.

## 3. Why we use it

| Purpose | Data | Basis, where the law asks for one |
|---|---|---|
| Sign you in and keep your account | GitHub identity, agreement records | Contract |
| Decide on your application | Application, account signals | Contract; our legitimate interest in keeping out abuse |
| Run your land's agents | Repository content, conversation and work records, transcripts | Contract |
| Count and limit spend | Usage and credit records | Contract |
| Keep the Service secure, investigate abuse, enforce the AUP | All of the above, service logs | Legitimate interest; legal obligation |
| Support you and fix problems | Whatever the problem needs, usually transcripts, recordings and logs | Contract; legitimate interest |
| Tell you about your application, land, purse and changes to these documents | Email address | Contract |

We do not use your data for advertising, sell it, or use it to train AI models.

## 4. Who can see it

- **You and your land's people** see your land's conversations, plans, tasks, decisions,
  agent status and purse, according to their role. Nobody in a land sees an agent's terminal.
- **People in other lands** see nothing of yours.
- **The operator** (us) can see everything in every land, including transcripts and terminal
  recordings. We look when running, securing or supporting the Service needs it.
- **Our providers** process data for us, under their terms, only to provide their service:

| Provider | What they get | Why |
|---|---|---|
| Anthropic, PBC | The prompts agents send: your messages, repository content, tool output | The AI model that does the agents' work |
| Amazon Web Services (United States, us-east-1) | Everything we store | Hosting, storage, backups, email (Amazon SES), and text embeddings for search (Amazon Bedrock) where enabled |
| GitHub, Inc. | What agents push to your repository; the App's API calls | Your repository and sign-in |
| Slack Technologies | Notices to the operator about your application, support requests and abuse cases, which can quote what you wrote | How the operator is alerted. Your land's conversations are never mirrored to Slack. |

- **Others, only when we must:** to comply with the law or a valid legal request, to protect
  someone's safety, to deal with abuse under the [AUP](aup.md#6-what-happens-on-abuse), or as
  part of a transfer of the Service to a successor, who must keep this policy's promises.

Your data is stored and processed in the United States. If you are outside it, your data is
transferred there.

## 5. How long we keep it

| What | While your land is open | After it closes |
|---|---|---|
| Repository content in an agent's workspace | Only while that agent runs | Gone with the agent |
| Conversation and work records | Kept | Deleted after {{RETENTION_DAYS}} days |
| Agent transcripts and terminal recordings | Kept | Deleted after {{RETENTION_DAYS}} days |
| Membership and invites | Kept | Deleted after {{RETENTION_DAYS}} days |
| Per-request spend records | Kept | Deleted after {{RETENTION_DAYS}} days |
| Credit ledger and agreement records | Kept | Kept {{RECORDS_RETENTION}}, then deleted |
| Records of an abuse case | Kept | Kept {{RECORDS_RETENTION}} after the case closes, then deleted |
| Service logs | 30 days | 30 days |
| Metrics (counts and timings, no content) | 90 days | 90 days |
| Database and storage backups | Up to 35 days | Up to 35 days after the deletion above |

| What | Retention |
|---|---|
| A denied or withdrawn application | {{APPLICATION_RETENTION}} days after the decision, then deleted |
| Your GitHub identity | While you belong to a land or have an open application, then deleted with the last of the above, unless kept with agreement or abuse records |

Your repository on GitHub is yours and is not deleted. Uninstall the Gardenio Lands GitHub App
to remove our access.

## 6. Your choices and rights

You can ask us, in `#support` or at {{SUPPORT_EMAIL}}, to:

- give you a copy of your data, including your land's conversations and records;
- correct something wrong;
- delete your account or close your land early;
- stop using your data for a purpose you object to.

We answer within 30 days. Depending on where you live (for example under the GDPR in the EU and
UK, or the CCPA in California), you may have further rights, including to complain to your data
protection authority. We may keep what the law, an open abuse case, or a dispute requires, and
tell you when we do.

Removing a member from a land stops their access within 5 minutes; what they wrote in the land
stays part of the land's records.

## 7. Security

Each land is isolated from the others: its agents run in their own network-fenced space, with
a GitHub token for its one repository and an AI key for its own purse. Stored agent sessions
are encrypted at rest. Only the operator can see agent terminals. No system is perfectly
secure; if a breach affects your data, we will tell you without undue delay and as the law
requires.

## 8. Children

The Service is not for anyone under 18, and we do not knowingly collect their data. If you
believe a child has signed up, tell us and we will delete the account.

## 9. Changes

We publish every change as a new version. For a material change we email land owners and ask
every user to accept the new version on their next visit.

## 10. Contact

{{ENTITY}}, {{CONTACT_ADDRESS}}. Email: {{SUPPORT_EMAIL}}.
