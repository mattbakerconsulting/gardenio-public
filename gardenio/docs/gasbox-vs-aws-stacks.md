# Gasbox (GiB) vs the AWS stacks — a topology disambiguation

> **Why this doc exists.** Three unrelated efforts kept getting conflated in
> chat, beads, and hallway advice — most damagingly, a predecessor Slack message
> that called **Gardenio-in-a-Box "the new prod cluster."** That is wrong on two
> counts (GiB is not a cluster, and it is not prod), and the mistake propagated.
> This is the authoritative, one-page glossary that keeps the names straight.
> It is intentionally short; the deploy *mechanics* live in
> [`release-pipeline.md`](release-pipeline.md) and are not repeated here.
>
> **Origin:** bead ka-0000007876 (disambiguation task, operator
> matthew.baker@acme.example). Supersedes the June-17 Slack disambiguation, which
> predates the 2026-06-18 promote-in-place decision and the 2026-07-05 P4.4
> prod cutover. The short-form version of this doc ships as project advice
> **ka-0000005379**.

---

## TL;DR

There are **two entirely different things** people call "gardenio," and the AWS
one has **three tiers** (one live, two legacy). Don't cross the streams:

| Name | What it is | Prod? |
|---|---|---|
| **Gasbox / GiB / Gardenio-in-a-Box** | A *portable product*: the whole platform installed on **one EC2 box** with an **in-box k3s** and **zero external cloud deps**. A demo/eval/BYO artifact. | **No.** Not a ACME cluster, not prod. |
| **`prod-gardenio-eks`** (acct `123456789012`) | The **live ACME production** gardenio fleet. | **Yes — this is prod.** |
| `poc-gardenio-eks` (acct `123456789012`) | **Legacy** POC-flavoured cluster, being retired (Phase C). | No (legacy). |
| `america-e2e-eks` (acct `234567890123`) | The **original** prod, now the **frozen rollback source**. | No (frozen ex-prod). |

The single most common error: **"the new prod cluster is GiB / the POC account."**
It isn't. GiB is a portable box. The "POC account" (`123456789012`) was
**promoted in place to BE prod** — it *is* `prod-gardenio-eks` now, not a
future-prod-in-waiting.

---

## 1. Gasbox / GiB / Gardenio-in-a-Box — a portable product, NOT prod

**What it is:** a complete gardenio platform that installs on a **single EC2
instance** with an **in-box Kubernetes** (k3s) and **no other cloud
dependency** — no RDS, no S3, no Secrets Manager, no External Secrets / IRSA, no
Route53 / ACM, no EBS/EFS CSI, no external registry pull as a hard dep. Every
AWS-managed service has an in-box equivalent:

| AWS-managed (hosted stacks use) | In-box equivalent |
|---|---|
| RDS Postgres | in-box Postgres |
| S3 (sessions/recordings/transcripts) | in-box MinIO / local PV |
| Secrets Manager + ESO + IRSA | in-box-seeded k8s Secrets |
| Route53 + ACM | cert-manager + local CA / HTTP-01; local DNS |
| EBS/EFS CSI + StorageClass | local-path-provisioner (Retain) |
| Verified Access edge-auth | in-box Keycloak + traefik forwardauth |
| External registry pull | in-box `zot` registry |

**Names you'll see for the same thing:** "gasbox", "GiB", "Gardenio-in-a-Box",
"in-box", "inbox overlay", "BYO cluster". The historical demo endpoint was
`192.0.2.91` (a single k3s EC2 box).

**Where it lives in the repo:**
- Overlays: `deploy-values/gardenio-inbox.yaml`, `gardenio-inbox-slack.yaml`,
  `gardenio-inbox-import-only.yaml`, `gardenio-gvisor-inbox.yaml`,
  `gardenio-byo.yaml`.
- Docs: `gardenio/docs/inbox-*.md`, [`gardenio/docs/byo-cluster.md`](byo-cluster.md),
  `gardenio/docs/gardenio-in-a-box-restore.md`, `gardenio/docs/research/*in-a-box*`.
- Mega-epic **ka-0000001295** (CLOSED).

**What it is NOT:** it is **not** a ACME-hosted cluster, **not** the production
environment, and **not** "the new prod." It is a portable demo/eval/BYO artifact
that anyone can stand up on their own box. Its lifecycle, hosts, and accounts
have **nothing to do with** the AWS stacks in §2.

---

## 2. The AWS stacks — one live prod, two legacy tiers

All three below are **ACME-hosted EKS clusters**. They are the opposite of GiB:
fully cloud-native (RDS, S3, ESO/IRSA, Route53/ACM, EBS CSI, ECR). Deploy
mechanics for each are documented authoritatively in
[`release-pipeline.md`](release-pipeline.md); this section only fixes the
*identity* of each stack.

### 2a. `prod-gardenio-eks` — the LIVE production stack ✅

- **Cluster:** `prod-gardenio-eks` · **AWS account `123456789012`** · `us-east-1`.
- **Namespace:** `gardenio-system`. **Hosts:** `gardenio.example.com`.
- **Deployed by:** a targeted `terraform apply -target=module.gardenio` of
  `gardenio/terraform/composition/canonical-prod` (`prod-real.tfvars`,
  `env_name="prod"`) — **not** `helm upgrade`. The deploy **hard-fails if the STS
  account ≠ `123456789012`**.
- **History that trips people up:** account `123456789012` began life as the
  **"POC" account** (`poc-gardenio-eks`). Per the **LOCKED decision of
  2026-06-18**, that account was **promoted in place** to become production, and
  the **P4.4 cutover (2026-07-05, ka-0000006191)** moved the live prod stack onto
  `prod-gardenio-eks` in the `gardenio-system` namespace. So "the POC account" and
  "prod" are now **the same AWS account** — POC did not stay a separate
  pre-prod; it *became* prod.
- **"canonical-prod" / "customer-poc":** these are the names of the **Terraform
  composition** that builds this cluster (`composition/canonical-prod`), plus the
  research umbrella **ka-0000005972** (renamed "customer-poc"). They are the
  *lineage of the TF code*, not a separate future environment. The live env's
  `env_name` is simply **`prod`**. There is **no** unbuilt net-new "canonical-prod"
  account waiting in the wings — that plan (a from-scratch cluster in a dedicated
  new account) was **superseded** by the promote-in-place decision.

### 2b. Staging / CI ephemerals — ON the prod cluster

- Staging deploys, full E2E, and the `gardenio-rwx` singleton run as **per-instance
  ephemeral namespaces `gardenio-rwx-*`** — **on the same consolidated
  `prod-gardenio-eks` cluster** (acct `123456789012`).
- **Phase B (ka-0000002380, epic ka-0000007588)** retired the *separate*
  `poc-gardenio-eks` staging cluster and folded staging onto prod to kill its fixed
  cost. Ephemerals are namespace-isolated from the prod stack and reaped by
  `gardenio-rwx-reaper`. Hosts: `*.gardenio-rwx.gardenio.example.com`.
- Selected by the `GARDENIO_STAGING_HOST_DOMAIN=gardenio.example.com` CI variable,
  which picks the `-prod` overlays (`deploy-values/gardenio-rwx-prod.yaml`).

### 2c. Legacy / rollback tiers

- **`poc-gardenio-eks`** (same acct `123456789012`) + the `-poc` overlays
  (`gardenio-rwx-poc.yaml`), hosts `aws-poc.gardenio.acme.example` — the POC-flavoured
  staging cluster. **Valid only until Phase C retires it.** A rollback tier, not
  a live environment.
- **`america-e2e-eks`** (acct **`234567890123`**), hosts `e2e.example.com` —
  the **original** prod cluster. Now the **frozen rollback source**, kept for
  emergency fallback only.

---

## 3. Fast disambiguation — which is it?

- Someone says **"gasbox / GiB / in-a-box / the box / BYO"** → §1, the portable
  single-EC2 product. **Never prod.**
- Someone says **"prod / production / the live fleet"** → `prod-gardenio-eks`,
  acct `123456789012` (§2a).
- Someone says **"the POC account / 123456789012"** → that account **is** prod
  now (§2a); the *old* `poc-gardenio-eks` cluster on it is legacy staging (§2c).
- Someone says **"canonical-prod / customer-poc"** → the TF composition +
  research umbrella that *builds* prod (§2a). Not a separate environment.
- Someone says **"america-e2e / 234567890123 / e2e.example.com"** → the
  original prod, now **frozen rollback** (§2c).
- Someone calls **GiB "the new prod cluster"** → **wrong on both counts.** Point
  them here.

---

## 4. Bead / doc index

| Thing | Key beads | Key files |
|---|---|---|
| Gasbox / GiB | ka-0000001295 (mega-epic, CLOSED) | `deploy-values/gardenio-inbox*.yaml`, `gardenio-byo.yaml`; `gardenio/docs/inbox-*.md`, `byo-cluster.md` |
| POC→staging repurpose | ka-0000005616 (CLOSED) | `deploy-values/gardenio-rwx-poc.yaml` |
| canonical-prod / -AI research | ka-0000005972 (OPEN, research-only) | `gardenio/terraform/composition/canonical-prod/`, `docs/research/canonical-*.md`, `docs/adr/0002-*` |
| Phase B (retire poc, fold staging onto prod) | ka-0000002380, epic ka-0000007588 | `deploy-values/gardenio-rwx-prod.yaml` |
| P4.4 prod cutover | ka-0000006191 | `prod-real.tfvars`, [`release-pipeline.md`](release-pipeline.md) |
