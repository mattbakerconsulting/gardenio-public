# DEVOPS-812 — On-prem GitLab instance + CI runners setup runbook

> JIRA: [DEVOPS-812](https://acme.atlassian.net/browse/DEVOPS-812) ·
> Bead: ka-0000008100 ·
> Sibling tickets: [DEVOPS-798](https://acme.atlassian.net/browse/DEVOPS-798)
> (end-to-end staging env) · [DEVOPS-814](https://acme.atlassian.net/browse/DEVOPS-814)
> (enable Gas Boat against the new Jira cluster).
>
> **Status of this doc:** operator runbook for the GitLab-instance-and-runners
> slice agreed at the **June 16th AI Driven Support & Operations** meeting. The
> goal is a self-hosted GitLab instance with its own CI runners, isolated from
> `gitlab.com/example-org`, so Gas Boat can be wired up end-to-end (Jira + GitLab)
> in an on-prem environment **with zero risk to production**.
>
> As with the DEVOPS-798 runbook, **infrastructure provisioning stays manual**
> (helm/kubectl click-ops owned by a human operator). This doc records the
> evaluation of the two approaches, the exact steps, the **real artifacts that
> already exist in this repo**, the named owners, and the open decisions. It
> does **not** run `helm install`, `kubectl apply`, or any terraform itself.

---

## 0. TL;DR

| | |
|---|---|
| **What** | A self-hosted GitLab instance + connected CI runners, off the production `gitlab.com/example-org` org. |
| **Why** | Isolated on-prem surface to test the Gas Boat ↔ GitLab integration end-to-end alongside the new Jira cluster (DEVOPS-814) and the staging env (DEVOPS-798). |
| **Owner** | Matthew Baker (infra provisioning + helm config), with Sky Bennett (alternative-approach evaluation + sync). |
| **Key finding** | **This is not greenfield.** A self-hosted GitLab + in-cluster Kubernetes runners already exist as committed artifacts under `deploy-values/gitlab-dark/` ("gitlab-dark"), with a **runner baseline already e2e-green** (trivial pipeline GREEN, #25/#26). DEVOPS-812 should **reuse gitlab-dark**, not stand up a parallel instance. |

### The two approaches the ticket asks us to evaluate

The ticket records Sky Bennett's note that *"a test group within GitLab may be
sufficient as an alternative approach."* Both are evaluated in **§1**.

- **Approach A — full self-hosted GitLab instance** (the ticket's primary ask).
- **Approach B — a test group on existing GitLab** (Shannon's lighter alternative).

**Recommendation: Approach A, reusing the existing `gitlab-dark` instance** — it
already satisfies the acceptance criteria's "own CI runners" + full isolation
requirement, and the heavy lifting (chart values, runner deploy, scaling/cost
guardrails) is already done. See §1 for the full trade-off.

---

## 1. Approach evaluation — full instance vs. test group

| Dimension | **A. Self-hosted GitLab instance** *(recommended)* | **B. Test group on existing GitLab** |
|---|---|---|
| Isolation from production | **Full** — separate control plane, DB, object storage, auth. No path to `gitlab.com/example-org/*`. | **Partial** — same `gitlab.com` tenant; relies on group/project permissions, not a hard boundary. Webhooks, tokens, and runners share the production surface. |
| "Own CI runners" (an explicit acceptance criterion) | **Yes** — dedicated in-cluster Kubernetes-executor runners, isolated namespace + cost caps. | Possible but shares the production runner fleet unless you register a group-scoped runner; still on the production instance. |
| Blast radius of a Gas Boat misfire | Contained to the dark instance; tickets/MRs/webhooks are throwaway. | A bad automated action (MR, comment, reviewer assignment) lands on the **real** `gitlab.com` org. |
| Setup cost | **Already largely built** (see §2) — the marginal cost is provisioning + wiring, not greenfield. | Low to start, but every guardrail (runner isolation, token scoping, webhook target) must be re-reasoned against production. |
| Fidelity for end-to-end testing | High — exercises MR creation, webhooks, pipeline triggers against a real-but-disposable GitLab. | High for API flows, but you cannot freely break/replay without touching production data. |

**Why A wins here:** the acceptance criteria require *own CI runners* and a
genuinely *isolated on-prem environment*. Approach B (test group) cannot give a
hard isolation boundary — it shares the production tenant, tokens, and webhook
surface, which is exactly the production risk the meeting wanted to avoid (the
DEVOPS-798 runbook §2 makes the same "never target production GitLab" point). And
critically, **Approach A is not greenfield**: the `gitlab-dark` instance and its
runners already exist as committed, partly-live artifacts. Choosing B would mean
*more* net work (re-deriving guardrails against production) for *less* isolation.

> Approach B remains a reasonable **fallback** if the operators decide the
> in-cluster GitLab is too heavy to maintain for this purpose — in which case
> use a dedicated, locked-down group + a group-scoped runner, and point the
> staging Gas Boat's `gitlabBridge.gitlab.groupID` at it. The §4 wiring and §5
> smoke test are approach-agnostic.

---

## 2. The existing self-hosted GitLab + runners (`deploy-values/gitlab-dark/`)

DEVOPS-812's deliverable already exists as committed Helm/kubectl artifacts.
**Nothing is auto-applied** — `helm install` is gated on the operator
provisioning decision (Matthew Baker). `deploy-values/gitlab-dark/` is onprem
GitLab Runner infra excluded from this repo's 2026-09-16 export and
decommissioned (decision gd-5oTzB6CSay); the paths below are named for
reference only, not linked. The pieces:

| Artifact | What it is | Status |
|---|---|---|
| `gitlab-dark/values-rwx.yaml` | Values overlay for the upstream `gitlab/gitlab` cloud-native chart (CE), targeting the `gardenio-rwx` EKS cluster. External RDS (scoped logical DB), bundled Redis, S3 object storage + registry, Keycloak OIDC. | DRAFT values |
| `gitlab-dark/ingressroute.yaml` | Front door: `gitlab-dark.gardenio-rwx.e2e.example.com` → webservice `:8181` (UI + API + git-over-HTTP). | committed |
| `gitlab-dark/runner/` | The in-cluster `gitlab/gitlab-runner` (Kubernetes executor) — **this is the "CI runners" acceptance criterion.** | **C1 baseline e2e-green** |
| `gitlab-dark/registry/` | In-cluster S3-backed registry + pull-through cache (avoids Docker Hub 429s). | drafted |
| `gitlab-dark/EXTERNAL-DEPS-STRATEGY.md` | Decision framework for external deps en route to real-green CI. | committed |

### 2.1 The CI runners — the heart of DEVOPS-812

The runner artifacts (`deploy-values/gitlab-dark/runner/`) directly satisfy *"CI
runners are configured and connected to the instance."* Two layers:

- **C1 baseline** (`values-c1-baseline.yaml`) —
  the conservative bring-up that is **already live and e2e-green**: a trivial
  `.gitlab-ci.yml` job on `root/c1-runner-smoke` ran on the Kubernetes executor
  and went **GREEN** (pipeline #25, no `allow_failure`).
  - `concurrent = 4`, `check_interval = 3s`, metrics on `:9252`.
  - Executor: Kubernetes, namespace `gitlab-dark-runner`, `privileged = false`,
    amd64-pinned build pods, `docker-hub-registry` pull secret.
  - Points at the in-cluster webservice DNS — **no egress**:
    `gitlabUrl: http://gitlab-dark-webservice-default.gitlab-dark.svc:8181/`.
- **C5 scaling overlay** (`values.yaml` + `SCALING.md`) — the
  cost/concurrency target per operator decision **`ka-0000000794` (`ceiling16`)**:
  `concurrent = 16`, a dedicated `gitlab-dark-ci` Karpenter NodePool, a
  ceiling-sized ResourceQuota (`limits.cpu = 16`), and `:9252` cost alerts.
  Applied with `./install.sh --scale` *after* the baseline is live.

> **GitLab 18 auth-token note:** GitLab 18 removed the legacy registration-token
> flow. The runner verifies a `glrtr-…` **runner authentication token** at boot;
> if that token is deleted server-side the manager crashloops with
> `403 Verifying runner... is removed`. Re-mint with
> `runner/install.sh --token`.

### 2.2 Deploy procedure (operator-run; do NOT run from an agent pod)

These are the human-owned steps. The runner `install.sh`
is idempotent.

1. **Provision the GitLab control plane** (Matthew Baker): out-of-band populate
   the AWS Secrets Manager source secrets the `ExternalSecret`s expect
   (`gitlab-dark-psql`, `gitlab-dark-object-storage`, `gitlab-dark-registry-storage`,
   `gitlab-dark-oidc`, `gitlab-dark-initial-root-password`, `gitlab-dark-tls` —
   see `gitlab-dark/README.md`),
   then `helm upgrade --install` the `gitlab/gitlab` chart with `values-rwx.yaml`.
   Record the external URL — call it `STAGING_GITLAB_URL`
   (`https://gitlab-dark.gardenio-rwx.e2e.example.com`).
2. **Deploy the runner** (Matthew Baker): from `deploy-values/gitlab-dark/runner/`,
   run `./install.sh` (baseline) — it stands up the namespace/RBAC/quota/limits/
   netpol, mints a fresh runner auth token against gitlab-dark, and installs the
   chart. Optionally `./install.sh --scale` once the baseline is green.
3. **Verify the runner is connected**: in the dark GitLab Admin → CI/CD →
   Runners, confirm the instance runner shows online; re-run the trivial
   smoke pipeline (`root/c1-runner-smoke`) and confirm GREEN.

> **Guardrail (matches DEVOPS-798 §2):** keep this entirely off production
> GitLab. The staging Gas Boat must only ever target `STAGING_GITLAB_URL`,
> never `gitlab.com/example-org/*`.

---

## 3. What Gas Boat needs from this instance (hand-off to DEVOPS-814)

To wire the **new** Gas Boat instance (DEVOPS-814, bead ka-0000006371) to this
GitLab, the gitlab-bridge needs three things from §2:

1. `STAGING_GITLAB_URL` — the dark GitLab base URL.
2. A **bot user + access token** with `api` scope (the bridge reads MRs, posts
   comments, assigns reviewers).
3. The **group/project** path(s) the bridge should watch.

---

## 4. Wiring Gas Boat → this GitLab (the `gitlabBridge` knobs)

The staging Gas Boat instance reads these helm keys (see the production block in
`deploy-values/gardenio.yaml` `gitlabBridge:`).
For the dark/staging instance, override:

| Key | Production value | Dark/staging override |
|---|---|---|
| `gitlabBridge.gitlab.baseURL` | `https://gitlab.com` | `STAGING_GITLAB_URL` |
| `gitlabBridge.gitlab.secretName` | `gardenio-gitlab-token` | the dark bot-token secret |
| `gitlabBridge.gitlab.groupID` | `14176856` | the dark group ID (Approach A) **or** the test-group ID (Approach B) |
| `gitlabBridge.gitlab.botUsername` | `gardeniobot` | the dark bot username |
| `gitlabBridge.gitlab.webhookSecretName` / `…SecretKey` | `gitlab-webhook-secret` | the dark webhook secret |

Then configure the **webhook** from the dark GitLab project → the staging
gitlab-bridge endpoint (MR events, note events, pipeline events). The bridge
service listens on port `8092` and is fronted by an ingress host you set per
instance.

> The full DEVOPS-814 wiring lives in that ticket's deliverable; this runbook
> only owns the GitLab+runner side and the contract handed across.

---

## 5. End-to-end validation (acceptance criterion: full flow validated)

Run once §2–§4 are live (Jira side per DEVOPS-814/798):

1. Create a curated, **synthetic** sample ticket in the new Jira cluster's
   sandbox project (no real study/site/PHI data — advice
   [ka-0000005335](https://gardenio.e2e.example.com/beads/ka-0000005335)).
2. Confirm the staging jira-bridge ingests it (a bead appears in the staging Gas
   Boat) and severity judgment fires.
3. Drive an action through to the **dark GitLab** — an MR + webhook round-trip —
   and confirm the **pipeline runs on the dark runner** (a job lands on the
   Kubernetes executor and goes GREEN, as in the C1 smoke proof).
4. Confirm Gas Boat reads the MR back (comment/reviewer assignment via the bot
   token) and posts lifecycle events.
5. **Delete** the sample ticket / tear down the MR and confirm clean teardown —
   then repeat. The whole point is a replayable loop with zero production impact.

---

## 6. Acceptance criteria → status

| Acceptance criterion | Status | Where |
|---|---|---|
| On-prem GitLab instance running and accessible | **Artifacts ready; awaiting operator `helm install`** | §2, `gitlab-dark/values-rwx.yaml` + `ingressroute.yaml` |
| CI runners configured and connected | **C1 baseline e2e-green; C5 ceiling16 overlay ready** | §2.1, `gitlab-dark/runner/` |
| Gas Boat can connect to this GitLab | **Wiring documented; lands with DEVOPS-814** | §3–§4, `gitlabBridge.*` |
| End-to-end flow (Jira + GitLab + Gas Boat) validated | **Smoke procedure defined; runs once DEVOPS-798/814 live** | §5 |

---

## 7. Open items / blockers

| Item | Owner | State |
|---|---|---|
| `helm install` of the dark GitLab control plane (AWS SM secrets populated) | Matthew Baker | pending operator decision |
| Runner baseline `./install.sh` re-confirmed live in current cluster | Matthew Baker | C1 was green #25/#26; re-verify |
| Approach A vs B final call (instance vs test group) | Matthew Baker + Sky Bennett | **decision** — this runbook recommends A (reuse gitlab-dark) |
| Bot user + `api`-scope token minted on dark GitLab | Matthew Baker | pending §2 |
| Staging Gas Boat instance wired (`gitlabBridge` overrides + webhook) | Matthew Baker | pending (DEVOPS-814) |
| Matthew Baker ↔ Sky Bennett sync to spin up the Gas Boat instance | both | scheduled (per meeting) |

> This runbook is the **agent-deliverable slice** of DEVOPS-812: it evaluates the
> two approaches, maps the acceptance criteria onto the existing `gitlab-dark`
> artifacts, and records the exact operator steps + wiring contract. The actual
> `helm install` / `kubectl apply` is human-owned click-ops requiring elevated
> cluster access, and is deliberately **not** automated here.
