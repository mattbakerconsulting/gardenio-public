# Lands legal documents

The three documents a stranger accepts before applying for a land: step 7.1 (gd-fyboiKszlg)
of the [Track B spec](../../superpowers/specs/2026-10-19-lands-track-b-design.md), §2.6.

| File | Document | Served at (step 7.5) |
|---|---|---|
| [terms.md](terms.md) | Terms of Service | `/lands/terms` |
| [privacy.md](privacy.md) | Privacy Policy | `/lands/privacy` |
| [aup.md](aup.md) | Acceptable Use Policy | `/lands/aup` |

All three are **drafts**. This directory holds the text only. Serving it and recording
acceptance is step 7.5. **Private beta (owner decision 2026-10-25, thread gd-lnDFBXRDEN):**
the first external lands (Open day, step 11.4) are a private beta for invited people, with no
money taken from strangers. They accept the draft set as it stands, `{{PLACEHOLDER}}`s marked.
The legal review (11.0 gd-ZjHdUWdNJI) and the placeholder fill (7.1b gd-c0AgIKuYrG) happen
before the payments decision (11.6 gd-cjItlf1QKP), not before Open day. The SLA ([../sla.md](../sla.md), step 10.9) is a draft of its own, outside this set, and is
deferred: there is no SLA for Open day (owner decision gd-yjw85aEUao), so Terms §8.1 keeps "no
promise". Publishing one later (step 11.6) changes §8.1 to name it, a new version of the set. The incident process and support
runbook are operator documents (10.9 and 11.1).
None of this is legal advice: a lawyer reads the set before any paying tenant (step 11.0, gating 11.6).

## Versions

Each document starts with YAML front matter:

| Field | Meaning |
|---|---|
| `doc` | `terms`, `privacy` or `aup` |
| `version` | The version id. A published version is the date it takes effect, `YYYY-MM-DD`. A draft is `YYYY-MM-DD-draft.N`. |
| `status` | `draft` or `published` |
| `effective` | The date a published version takes effect; `null` for a draft |
| `requires_reacceptance` | `true` when the change is material, so every user accepts again before going on (Terms §13) |

Rules:

1. **The three documents move together.** A change to any of them bumps all three to the same
   new `version`, so one id names the set. That is why the user bead needs only `terms_version`
   (step 7.4), and why a `plot_application`'s `terms_version` and `aup_version` are equal.
2. **Any change to the text is a new version.** Fixing a typo is still a new version, with
   `requires_reacceptance: false`.
3. **Published text is never edited in place.** Before changing a published version, copy each
   file to `archive/<doc>-<version>.md`, so the exact text anyone accepted can always be shown.
4. **Drafts are for test accounts and the private beta.** Step 7.9's check-in may accept a
   draft, and so may an invited private-beta tenant (step 11.3; owner decision 2026-10-25,
   thread gd-lnDFBXRDEN). Nobody pays, and no stranger gets a land, until `status: published`
   and every `{{PLACEHOLDER}}` is gone: that is the gate on the payments decision (11.6).

## Serving and acceptance (step 7.5)

The beads viewer serves the three documents at `/lands/terms`, `/lands/privacy` and `/lands/aup`
from an embedded copy in `gardenio/controller/cmd/beads-viewer/lands/legal/`, rendered as they are:
each `{{PLACEHOLDER}}` shows marked, never filled. `POST /lands/accept` records the set's `version`
as `terms_version` on the person's own user bead, so a new version asks everyone to accept again.
After changing any of the three files:

1. Bump `version` in all three (rules 1 and 2).
2. Copy them over the viewer's copy (`TestLandsLegalMirrorsDocs` fails until you do).
3. Add the new version's checksum to `landsLegalPins` in `lands_test.go`
   (`TestLandsLegalVersionPinsText` prints it, and fails if the text changed under an old version).
4. Commit no PNG: the Terms page is a golden, and the next window's bless PR re-shoots it
   (`gardenio/tests/gardenhub-browser/GOLDENS.md`, "Updating goldens").

## Owner decision

One decision, gd-IhEhhYTj4A, sets what only the owner can (spec §8, OQ2 folded in). It
resolved on 2026-10-19 as **private-ok**: the recommended values, with private repos allowed.
The credit unit comes from OQ1 gd-7sTt20714N (markup) and OQ1b gd-XQmEoHAqmp (one multiplier,
×1.5). Step 7.1b (gd-c0AgIKuYrG) puts these values into the text. Until then the text keeps its
`{{PLACEHOLDER}}`s. 7.1b is deferred with the legal review (owner decision 2026-10-25): the
private beta runs on the draft, and both run before the payments decision (11.6).

| Placeholder | Used in | Decided |
|---|---|---|
| `{{ENTITY}}` | All three | **Not answered yet.** The decision asked for it in free text, and none came with the answer. Recommended: the owner's registered business, if there is one, rather than the owner as an individual |
| `{{CONTACT_ADDRESS}}` | Terms, Privacy | **Not answered yet** (as above). Recommended: the entity's registered address |
| `{{JURISDICTION}}`, `{{VENUE}}` | Terms §14 | **Not answered yet** (as above). Recommended: where the entity is registered, and its courts |
| `{{LIABILITY_CAP}}` | Terms §11 | "the greater of the amount you paid us in the 12 months before the claim and US$100" (nothing is paid in v1, so US$100) |
| `{{RETENTION_DAYS}}` | Privacy §1, §5 | 30 |
| `{{RECORDS_RETENTION}}` | Privacy §5 | "for 2 years" |
| `{{APPLICATION_RETENTION}}` | Privacy §5 | 90 |
| `{{PRIVATE_REPOS_CLAUSE}}` | Terms §3.4 | Private repos **allowed** (OQ2): "Your repository may be public or private. If it is private, the operator can see its contents in your land (see section 5.4)." The private-repo paragraph in Privacy §2 stays. |
| `{{SUPPORT_EMAIL}}`, `{{ABUSE_EMAIL}}` | All three | `support@` and `abuse@` on the sender domain step 7.2's escalation sets up |
| `{{CREDIT_UNIT}}` | Terms §4.2 | From OQ1 gd-7sTt20714N (markup) and OQ1b gd-XQmEoHAqmp (single, ×1.5), not this decision: "US$1 of service; AI model usage is charged at 1.5× our provider's list price" |

The owner's answer also confirmed three commitments the drafts make, which bind how the
Service is built: no training on tenant content (Terms §5.5), operator visibility
of everything in a land (Terms §5.4), and the abuse order freeze, ask, close, report (AUP §6).

## Before publishing

Every row of Privacy §5 has to be true in the running system. On `main` today (2026-10-19) these
are not yet, and each needs work before `status: published`:

| Promise | Today | Owner of the fix |
|---|---|---|
| A closed land's beads, transcripts, recordings and spend records are deleted after `{{RETENTION_DAYS}}` days | Step 8.11 (gd-xw3t0xyCnJ) added the purge sweep ([provisioner.md "Purging"](../provisioner.md#purging-step-811)): it hard-deletes the land's beads and its agents' session objects (every version) `LANDS_RETENTION_DAYS` after close. It is off until the deployment sets `LANDS_PURGE_ENABLED`, gives the controller delete rights on the session prefix, and sets LiteLLM's `maximum_spend_logs_retention_period` to at most `{{RETENTION_DAYS}}` days: LiteLLM cannot delete one key's spend rows, so they age out globally. | Operator: the chart/values and IAM changes in the 8.11 escalation |
| Backups age out within 35 days | The AWS Backup vault keeps 35 days and the nightly Postgres dumps 7. The hourly `s3Backup` to S3 has no retention found. | Operator: a 35-day expiry on `s3://gardenio-hq-backups-123456789012/k3s-postgres/` (8.11 escalation) |
| Terminal recordings mask credentials | coop redacts by default, but `COOP_RECORD_REDACTION=off` turns it off (`coop/crates/cli/src/record.rs`) | Step 10.3's tenant pod allowlist: tenant pods never get that override |
| Spend records hold no prompt text | LiteLLM's `store_prompts_in_spend_logs` is unset, so its default (off) applies | Operator: pin it off in `litellm.extraGeneralSettings` (8.11 escalation) |
| Tenant conversation never reaches Slack; only operator notices do | Design B-D11 | Step 8.6 (`land_slack_mirror=off`) and the Open day live check (11.4) |
| Credits draw down at 1.5× the provider's list price (Terms §4.2, §4.3) | The spec's purse arithmetic (§3.4) and B-D7 assume at cost: `max_budget` on the land's LiteLLM key equals the grant. LiteLLM counts list-price dollars, so with ×1.5 the key's `max_budget` must be the grant ÷ 1.5. | Step 8.1 gd-UOT2lqRfmq (rate table) and 8.2 gd-QSzQBkO1lk (purse) |
| Data stays in AWS us-east-1 | The terraform composition and chart use us-east-1 | Confirm for the k3s cluster at the 10.7 security review |
