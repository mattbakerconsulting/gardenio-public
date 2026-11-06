# DEVOPS-798 — End-to-end Gas Boat staging env (JIRA · GitLab · Slack) setup runbook

> JIRA: [DEVOPS-798](https://acme.atlassian.net/browse/DEVOPS-798) ·
> Bead: ka-0000000200 ·
> Daily bundle: <https://gardenio.e2e.example.com/beads/bundle/ka-0000003199>
>
> **Status of this doc:** operator runbook for the *click-ops + config* work agreed
> at the **June 16th AI Driven Support & Operations** meeting. The goal is a safe,
> replayable end-to-end sandbox to exercise Gas Boat's severity judgment and
> support-automation flows against curated, non-sensitive sample tickets that can
> be created and deleted repeatedly — **with zero risk to production**.
>
> Per the key decisions in the ticket, **infrastructure provisioning stays manual
> (click-ops)** — this doc records the exact steps, the real config knobs each
> component reads, the named owners, and the open decisions; it does **not**
> automate Atlassian-admin, on-prem GitLab, or Slack-app provisioning.

---

## 0. TL;DR — five components, who owns each

| # | Component | Owner | Nature |
|---|---|---|---|
| 1 | **Atlassian Cloud Sandbox** (JIRA Service Management) | Lee Morgan | click-ops (admin.atlassian.com) + config export |
| 2 | **On-prem staging GitLab** + CI runners | Matthew Baker | infra provisioning |
| 3 | **New Gas Boat instance** wired to staging JIRA + staging GitLab | Matthew Baker | helm config (this repo's knobs) |
| 4 | **Slack integration** for the staging instance | Sky Bennett | Slack app (Socket Mode) + helm overlay |
| 5 | **Ticket markdown for ingestion testing** | Lee Morgan + Matthew Baker + Sky Bennett | doc deliverable + split decision |

Each section below is independently runnable; §3 (the new Gas Boat instance)
depends on §1 and §2 existing, and §4 layers onto §3.

### Guardrails (from the ticket's "Key Decisions Made")

- **Use the Atlassian cloud sandbox**, *not* local/Docker — the Data Center trial
  path is sunset by Atlassian and unavailable.
- **Limit config-as-code (Terraform) to stable settings** (SLAs, rotations). Keep
  **production-mimicking ticket create/delete and urgent schedule changes manual**
  (click-ops) — these are exactly the replayable, throwaway operations the sandbox
  exists for.
- This is a **staging** surface. Do **not** point any of it at production acme.net
  domains or the production JIRA/GitLab/Slack workspaces.

---

## 1. Atlassian Cloud Sandbox (JIRA Service Management) — *Lee Morgan*

The officially supported sandbox is a free, license-included copy of a Cloud
product, provisioned from the org admin console. This is the agreed path
(local/Docker Data Center trials are sunset).

### 1.1 Provision the sandbox

1. <https://admin.atlassian.com> → select the Acme org → **Products**.
2. Find **Jira Service Management** (and/or **Jira Software** if the support flows
   also touch it) → **··· → Create sandbox**. Sandbox creation is included with the
   existing paid license at no extra seat cost.
3. Note the sandbox base URL — it is a distinct site, typically
   `https://acme-sandbox.atlassian.net` (confirm the exact host the console
   assigns). **This host, not `acme.atlassian.net`, is what Gas Boat points at
   in §3.**

> ⚠️ The sandbox is a separate site with its own URL and its own API tokens.
> Treat its credentials as distinct from production — never reuse the prod
> `JIRA_API_TOKEN`.

### 1.2 Replicate production issue types & workflows

Sandbox copies the *current* production config at creation time, but config drifts.
To replicate accurately and keep it replayable:

- **Issue types / schemes:** Project settings → *Issue types* / *Issue type schemes*.
- **Workflows:** Settings → *Issues → Workflows* → export the production workflow(s)
  as XML/JSON and import into the sandbox project, or recreate the statuses +
  transitions to match (DEVOPS uses `Refine → … → Done`; PE has its own automation
  transitions — see [advice ka-0000003964](https://gardenio.e2e.example.com/beads/ka-0000003964)).
- **SLAs:** define **compressed** SLA targets in the sandbox JSM project (e.g.
  minutes/single-digit-hours instead of business-days) so the escalation path can
  be exercised in a single test session. SLAs/rotations are the **stable** settings
  the ticket explicitly allows to manage as config-as-code if desired.

> **Blocker (from the ticket):** Sky Bennett currently has insufficient JIRA admin
> permissions to inspect the production config directly. Lee Morgan (org admin) should
> either run the export, or grant Shannon the *Jira administrator* product role
> scoped to the sandbox so config inspection is unblocked without touching prod.

### 1.3 What Gas Boat needs from this step (hand-off to §3)

- Sandbox **base URL** (e.g. `https://acme-sandbox.atlassian.net`).
- A **bot account email** in the sandbox + an **API token** for it
  (id.atlassian.com → *Security → API tokens*, generated **while signed into the
  sandbox-scoped account**).
- The **project key(s)** to poll (e.g. a sandbox `DEVOPS`/`PE` mirror).

---

## 2. On-prem staging GitLab + CI runners — *Matthew Baker*

A dedicated GitLab instance for end-to-end flow testing (MR creation, webhooks,
pipeline triggers) without touching `gitlab.com/example-org`.

Checklist:

1. Stand up the GitLab instance (Omnibus / Helm / on-prem VM per the team's infra
   standard). Record its external URL — call it `STAGING_GITLAB_URL`.
2. Register at least one **CI runner** so pipelines actually execute (MR pipelines,
   webhook round-trips). A single shared runner is enough to prove the flow.
3. Create a **staging project** that mirrors the monorepo/gardenio layout closely
   enough to exercise MR creation and webhook delivery. It does **not** need full
   history — a minimal repo that pipelines can run against is sufficient for
   end-to-end flow testing.
4. Create a **bot user + Personal/Project Access Token** with `api` scope. This is
   what the staging Gas Boat's gitlab-bridge uses to read MRs, post comments, and
   assign reviewers.
5. Configure the **webhook** from the staging GitLab project → staging Gas Boat
   gitlab-bridge endpoint (MR events, note events, pipeline events).

> **Reminder:** keep this entirely off the production GitLab. The staging Gas Boat
> must only ever target `STAGING_GITLAB_URL`, never `gitlab.com/example-org/*`.

What Gas Boat needs from this step (hand-off to §3): `STAGING_GITLAB_URL`, the bot
token, and the staging project path.

---

## 3. New Gas Boat instance → staging JIRA + staging GitLab — *Matthew Baker*

The cheapest replayable staging surface for Gas Boat itself is a **gardenio-in-a-box**
(single EC2 box, on-box k3s, full stack) in the **AWS-poc account (123456789012)** —
the same body of work the team already uses for isolated proofs. See
[`gardenio/docs/gardenio-in-a-box-install.md`](../gardenio-in-a-box-install.md) and the
overview in [`gardenio/docs/inbox-slack-enable-overview.md`](../inbox-slack-enable-overview.md).
A fresh box keeps the sandbox cleanly separable from the production fleet (which
runs in the mgmt account) and is destroyable/rebuildable on demand — exactly the
replayability the ticket asks for.

> If the team prefers a namespace on an existing cluster instead of a box, the same
> helm knobs below apply — only the install path differs. The box is recommended
> because it is the most isolated and the easiest to tear down and recreate.

### 3.1 Point the jira-bridge at the **sandbox** JIRA

The jira-bridge reads its target purely from helm values + a K8s secret (no code
change). Real knobs (`gardenio/helm/gardenio/values.yaml`, `jiraBridge:` block):

| Value | Set to | Note |
|---|---|---|
| `jiraBridge.enabled` | `true` | turns the bridge on |
| `jiraBridge.jira.baseURL` | sandbox URL from §1.3 | e.g. `https://acme-sandbox.atlassian.net` |
| `jiraBridge.jira.projects` | sandbox project keys | e.g. `DEVOPS,PE` (poll list) |
| `jiraBridge.jira.secretName` | `gardenio-jira-bridge-creds` | K8s secret holding the token |
| `jiraBridge.jira.boatProjects` | `{project}={git_url}:{jira_prefix}` | `BOAT_PROJECTS` mapping: sandbox JIRA prefixes → boat project names |

The API token is consumed as env `JIRA_API_TOKEN`, mounted from secret key
`api-token` on the secret named by `jiraBridge.jira.secretName` (tokens never go in
git — see AUDIT §J6 and [advice ka-0000001317](https://gardenio.e2e.example.com/beads/ka-0000001317)):

```sh
NS=gardenio
kubectl -n "$NS" create secret generic gardenio-jira-bridge-creds \
  --from-literal=api-token='<sandbox-api-token>'
```

> The sandbox-vs-prod boundary is enforced entirely by `jiraBridge.jira.baseURL` +
> the token's home site. Set them to the sandbox; the staging instance can then
> create/transition/comment freely on throwaway sandbox tickets with no prod risk.

### 3.2 Point the gitlab-bridge at the **staging** GitLab

Set the gitlab-bridge's base URL and token to `STAGING_GITLAB_URL` + the §2 bot
token (gitlab-bridge `JIRA_BASE_URL` env is wired for enrichment; the GitLab target
and token are the gitlab-bridge's own config — confirm against the current
`gitlabBridge:` block in `values.yaml` at deploy time, as exact key names evolve).
Wire the staging project's webhook (§2.5) to this bridge.

### 3.3 Compressed SLAs for rapid escalation testing

Use **compressed** SLA / escalation timings so the escalation path fires within a
single test session instead of over business days. This lives in two places — keep
them consistent:

- **JIRA-side SLAs** — the compressed JSM SLA goals from §1.2.
- **Gas Boat-side timings** — gardener/escalation cadence (schedules, decision
  reflag windows). These are tunable per-instance; set them aggressively low for
  the sandbox so escalations surface quickly.

### 3.4 Bring it up

Install/upgrade the box's gardenio release with the jira + gitlab bridges enabled
and pointed at staging targets, then verify:

```sh
kubectl -n gardenio rollout status deploy/gardenio-jira-bridge   --timeout=120s
kubectl -n gardenio rollout status deploy/gardenio-gitlab-bridge --timeout=120s
# jira-bridge should log polling against the SANDBOX host, not prod:
kubectl -n gardenio logs deploy/gardenio-jira-bridge | grep -i 'acme-sandbox' | head
```

> If a bridge pod hits `CreateContainerConfigError`, its creds secret is missing or
> has the wrong key name (jira: `api-token`) — recreate per §3.1
> (cf. [advice ka-0000001317](https://gardenio.e2e.example.com/beads/ka-0000001317)).

---

## 4. Slack integration — *Sky Bennett*

Connect the staging Gas Boat to Slack via **Socket Mode** (outbound WebSocket — no
public webhook, no new inbound surface on the box). The full procedure already
exists and applies verbatim:

- **Overview:** [`gardenio/docs/inbox-slack-enable-overview.md`](../inbox-slack-enable-overview.md)
- **Step-by-step runbook:** [`gardenio/docs/inbox-slack-enable-runbook.md`](../inbox-slack-enable-runbook.md)
- **Overlay:** `deploy-values/gardenio-inbox-slack.yaml`

Sandbox-specific notes:

1. **Use a separate Slack app and a separate channel** for the staging instance so
   sandbox traffic never lands in the production `#monorepo` / `#daily-stand-up`
   channels. Create the app at <https://api.slack.com/apps> with the scopes + event
   subs in the runbook §1, enable Socket Mode, install, and invite it to a
   dedicated staging channel; grab the `C0…` channel id.
2. **Creds secret on the box** (runbook §2):
   ```sh
   kubectl -n gardenio create secret generic gardenio-slack-bridge-creds \
     --from-literal=bot-token='xoxb-…' --from-literal=app-token='xapp-…'
   ```
3. **Layer the slack overlay last** in `helm upgrade` and inject the channel id with
   `--set slackBridge.slack.channel=C0XXXXXXXXX` (runbook §3–§4).
4. **Map channel → project** on the staging project bead (runbook §5) and verify the
   bridge logs `Slack Socket Mode bot enabled` (runbook §6).

> **Prerequisite (from the ticket):** Shannon should first review the Gas Boat
> account itself; Slack connection is the gating prerequisite for the broader
> end-to-end testing.

---

## 5. Ticket markdown for ingestion testing — split decision

The team needs an improved markdown corpus (larger index, more keywords, expanded
summaries) for Gas Boat ingestion testing, and must decide **single combined file
vs split index + full content**.

### Recommendation: **split — a thin index file + a full-content file**

| Approach | Pros | Cons |
|---|---|---|
| Single combined file | one artifact to share; simplest to version | index and bodies grow together; large file is noisier to scan and re-ingest; harder to update one ticket without re-reading everything |
| **Split: index + full content** *(recommended)* | index stays small, keyword-dense, and cheap to scan/diff; full bodies live separately and can grow without bloating the index; updating one ticket touches only the content file; mirrors how Gas Boat already ingests (cheap index lookup → fetch full body on demand) | two files to keep in sync |

**Why split wins here:** the ingestion test specifically wants a *larger index with
more keywords* alongside *expanded summaries*. Those two goals pull in opposite
directions inside one file — a bigger index makes a combined file harder to scan,
and expanded bodies bury the index. Splitting lets each grow independently and
matches the index-then-fetch retrieval pattern. Keep them adjacent and
cross-linked (index rows reference content anchors) so they stay in sync.

**Suggested shape:**

- `ingestion-index.md` — one row per sample ticket: id, title, severity, a dense
  keyword line, and a one-line summary. This is the "larger index, more keywords"
  surface.
- `ingestion-tickets.md` — full expanded summary per ticket, anchored so index rows
  link straight to the body.

Both files must use **curated, non-sensitive synthetic** content only — no real
study names, site identifiers, customer data, or PHI
(cf. [advice ka-0000005335](https://gardenio.e2e.example.com/beads/ka-0000005335)).
This keeps the corpus freely create/delete-able against the sandbox.

> Decision owners: Lee Morgan + Matthew Baker + Sky Bennett. This runbook records the
> recommendation; the final call is theirs.

---

## 6. End-to-end smoke test (once §1–§5 are live)

1. Create a curated, synthetic sample ticket in the **sandbox** JIRA project.
2. Confirm the staging jira-bridge ingests it (a bead appears in the staging Gas
   Boat) and that Gas Boat's severity judgment fires.
3. Drive an action through to **staging GitLab** (e.g. an MR + webhook round-trip)
   and confirm the pipeline runs on the staging runner.
4. Confirm lifecycle + decision events post to the **staging Slack channel**.
5. **Delete** the sample ticket and confirm clean teardown — then repeat. The whole
   point is that this loop is replayable with zero production impact.

---

## 7. Open items / blockers to resolve

| Item | Owner | State |
|---|---|---|
| Sandbox provisioned + prod config exported/replicated | Lee Morgan | pending |
| Shannon's JIRA admin permission for config inspection | Lee Morgan (grant) | **blocker** |
| Licensing review scheduled | Lee Morgan | pending |
| On-prem staging GitLab + runner stood up | Matthew Baker | pending |
| Staging Gas Boat instance enabled + wired | Matthew Baker | pending (needs §1, §2) |
| Gas Boat account reviewed + Slack connected | Sky Bennett | pending |
| Markdown split approach finalized | Gray + Matt + Shannon | recommendation in §5; decision pending |

> These are coordination/click-ops items owned by named humans with elevated
> access (Atlassian org admin, on-prem infra, Slack workspace admin). This runbook
> exists so that whoever picks each up has the exact steps and the real Gas Boat
> config knobs in front of them.
