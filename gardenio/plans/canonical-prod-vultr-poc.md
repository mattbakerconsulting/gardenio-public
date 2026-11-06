# Canonical-prod Vultr POC — scope, cutline, and acceptance checklist

**Status:** plan-of-record (POC scoping, research-only — no implementation commits).
**Tracking bead:** ka-0000006197.
**Parent epic:** ka-0000006189 — "Add Vultr support to canonical-prod reference Terraform module (research + design)".
**Umbrella epic:** ka-0000005972 — "Canonical production gardenio cluster + reusable Terraform module".
**Predecessor docs:**
- `gardenio/plans/canonical-prod-round2-refinement.md` — plan-of-record for the 12 child epics under ka-0000005972.
- `docs/research/canonical-prod-batteries-included.md` — portable-by-default operator inventory.
- `docs/research/canonical-prod-networking.md`, `…-eks.md`, `…-stateful-services.md`, `…-dns-tls-cutover.md` — AWS-side designs this POC mirrors on Vultr.
- *(forthcoming, sibling tasks under ka-0000006189)* `docs/research/canonical-prod-vultr-provider-map.md` (ka-0000007377), `docs/research/canonical-prod-vultr-architecture-impact.md` (ka-0000002100), Vultr provider survey (ka-0000002019), portability-seam ADR (ka-0000004922). This POC plan **does not pre-empt** their conclusions; where it makes a concrete pick (e.g. "use in-cluster Postgres") it is for POC purposes only and is flagged in §6.

> **No-implementation guardrail (inherited from ka-0000005972 + advice ka-0000003177).** This document defines what a successful Vultr POC looks like. It does **not** authorize a `terraform apply`, a Vultr account purchase, a new state backend, or a `feat/canonical-prod-vultr/*` branch in `site-deployment`. Each is gated on the open decisions in §6.

---

## 1. TL;DR

The Vultr POC is the **portability proof** for the canonical-prod gardenio module. One person, one afternoon, one region — `terraform apply` against a fresh Vultr account stands up a gardenio-ready cluster reachable at a real DNS name with valid TLS, and a human can tick every box in §7 to call it a success.

**In scope.** A single-region Vultr environment running:

- **Vultr Kubernetes Engine (VKE)** cluster — one node pool, no HA control-plane tuning beyond defaults.
- **In-cluster Postgres with pgvector** (the gardenio chart's default `postgres.enabled: true` path using the `pgvector/pgvector:pg17` image). Backs both the beads daemon and gardenio controller.
- **In-cluster NATS** (the gardenio chart's bundled NATS — `nats.enabled: true`).
- **Gardenio controller + coopmux + slack-bridge** — the standard gardenio Helm chart, deployed with a Vultr-flavored values overlay.
- **Portable platform addons** — external-dns (Vultr provider), external-secrets (no backing store — see §3.5), cert-manager (HTTP-01 issuer), VictoriaMetrics stack (vm-operator + vmsingle + vmagent).
- **Vultr DNS** as the public zone provider for one subdomain (e.g. `vultr-poc.gardenio.app.acme.example`), with NS delegation from the existing `acme.example` apex.
- **Vultr Load Balancer**, provisioned by the VKE CCM in response to the gardenio `Service type=LoadBalancer`.
- **Vultr Object Storage** for `gardenio-sessions` (S3-API; `COOP_S3_ENDPOINT` overrides AWS endpoints).
- **TLS via Let's Encrypt** (cert-manager HTTP-01) on the public gardenio hostnames.

**Out of scope (cutline — §4).** No multi-region. No HA managed-PG read replicas. No production-grade secrets backend (no Vault, no Vultr-side KMS). No migration from `america-e2e-eks`. No third cloud target. No CI dispatch of `terraform apply` — apply runs from an operator's workstation only.

**Done when.** Every box in §7 is ticked by a human reviewer working from a fresh checkout, and one `gb decision` ("Vultr POC ready to leave research and enter implementation") resolves `approve` per the parent epic's done-when.

---

## 2. POC architecture (one diagram, no surprises)

```
                                ┌──────────────────────────────────────────────────┐
                                │              Vultr region (e.g. EWR)             │
                                │                                                  │
   acme.example (apex, Route53) ───► │  Vultr DNS zone:                                 │
   NS delegate:                 │    vultr-poc.gardenio.app.acme.example                 │
   vultr-poc.gardenio.app.       │                                                  │
   acme.example → Vultr NS           │  ┌────────────────────────────┐                  │
                                │  │ VKE cluster (1 node pool)  │                  │
                                │  │                            │                  │
                                │  │  ns: gardenio-system        │                  │
   ┌───────────────┐            │  │   ├─ gardenio-controller    │                  │
   │ Slack         │◄───────────┼──┼─◄─ slack-bridge           │                  │
   └───────────────┘            │  │   ├─ coopmux               │                  │
                                │  │   ├─ in-cluster Postgres   │                  │
                                │  │   │   (pgvector image)     │                  │
                                │  │   └─ in-cluster NATS       │                  │
                                │  │                            │                  │
                                │  │  ns: cert-manager          │                  │
                                │  │  ns: external-dns          │──► Vultr DNS API │
                                │  │  ns: external-secrets      │  (no backend)    │
                                │  │  ns: victoria-metrics      │                  │
                                │  │                            │                  │
                                │  │   Service type=LoadBalancer│                  │
                                │  │   ▲                        │                  │
                                │  └───┼────────────────────────┘                  │
                                │      │                                           │
                                │      ▼                                           │
                                │  Vultr Load Balancer (public)                    │
                                │      │                                           │
                                │      ▼                                           │
                                │  reserved IP ──► A record at                     │
                                │                  gardenio.vultr-poc.gardenio.app.  │
                                │                  acme.example (managed by external-   │
                                │                  dns)                            │
                                │                                                  │
                                │  Vultr Object Storage bucket                     │
                                │    gardenio-sessions (S3-API,                     │
                                │    accessed via COOP_S3_ENDPOINT)                │
                                └──────────────────────────────────────────────────┘
```

**Components are the same code paths the AWS canonical-prod design uses.** The seam is at the provider/resource layer: VKE replaces EKS, Vultr Object Storage replaces S3, Vultr LB replaces NLB, Vultr DNS replaces Route53. Everything *inside* the cluster — gardenio chart, addons, beads schema, agent images — is unchanged.

---

## 3. What gets provisioned

The POC is one Terraform root module, one workspace, one state file (operator-local backend for POC; remote-state decision parked until the portability-seam ADR lands — see ka-0000004922).

### 3.1 Vultr resources (Terraform-managed)

| Resource | Vultr type | Count | Notes |
|---|---|---|---|
| Kubernetes cluster | `vultr_kubernetes` | 1 | Latest VKE-supported minor ≥ 1.29 (matches canonical-prod EKS floor). |
| Node pool | `vultr_kubernetes_node_pools` (inline or sibling) | 1 | 3 × `vc2-4c-8gb` or `vhf-3c-8gb` (POC sizing — not the production shape). |
| Public DNS zone | `vultr_dns_domain` | 1 | `vultr-poc.gardenio.app.acme.example`. NS records delegated from `acme.example` by an out-of-band Route53 change (operator step, §6 D-3). |
| Object Storage bucket | `vultr_object_storage` + bucket create (provider's bucket support is uneven — see §6 D-4) | 1 | `gardenio-sessions-vultr-poc`. |
| Reserved IP (optional) | `vultr_reserved_ip` | 0–1 | Used only if the LB does not give a stable IP for the lifetime of the POC. Default: rely on LB-assigned IP and let external-dns track it. |
| VPC 2.0 | `vultr_vpc2` | 1 | Single CIDR (Vultr VPC 2.0 only allows one CIDR per region per VPC). `10.60.0.0/16` (canonical-prod uses `10.50.0.0/16`; non-overlapping for the eventual third-cloud comparison). |

**Not** provisioned by Terraform in the POC:

- Vultr account / billing / SSH keys / API token — operator pre-creates these. The Terraform module reads `VULTR_API_KEY` from env.
- An IAM-equivalent role for the cluster — Vultr does not have IRSA; the POC uses **API-token secrets in-cluster** (one for external-dns, one for object-storage access). This is exactly the kind of regression the portability work should highlight, and it is captured as a divergence in §5.

### 3.2 Cluster-internal install order (Helm)

Mirrors the AWS install order (see `docs/research/canonical-prod-platform.md` §3) with Vultr substitutions. All releases run from `terraform-managed` Helm provider (POC) — not Argo, not flux, no GitOps loop in the POC.

1. **CRDs** — cert-manager, external-secrets, traefik (if used; POC defaults to plain `Service type=LoadBalancer` and skips traefik for now), victoria-metrics-operator.
2. **cert-manager** (operator + CRDs), then `ClusterIssuer` (HTTP-01 against the LB, Let's Encrypt prod endpoint).
3. **external-dns** (provider `vultr`, zone filter `vultr-poc.gardenio.app.acme.example`, API token from a K8s Secret).
4. **external-secrets** (operator only — no backing store in the POC; existence of the operator proves the addon is portable).
5. **VictoriaMetrics stack** — vm-operator + one VMSingle + a VMAgent that scrapes the gardenio namespace. No Grafana in POC; access via `kubectl port-forward` or VM UI.
6. **gardenio chart** (`helm/gardenio/`) with the Vultr overlay (see §3.6).

### 3.3 Postgres + pgvector

The POC uses **in-cluster Postgres with the `pgvector/pgvector:pg17` image** that the gardenio chart already ships (`gardenio/helm/gardenio/values.yaml:1013-1056`). This is a deliberate POC choice, not a recommendation for production:

- **Why in-cluster?** Vultr Managed Postgres (VMPG) ships PostgreSQL but the **pgvector extension is not on Vultr's published extension list** at time of writing. The beads embedding backend hard-requires pgvector (see `gardenio/helm/gardenio/values.yaml:1271-1280`). Until VMPG-with-pgvector is confirmed by a sibling research task, the POC stays in-cluster.
- **Why this is acceptable for a POC.** The cutline (§4) explicitly excludes HA managed-PG. In-cluster Postgres lets the POC make progress without blocking on VMPG pgvector confirmation.
- **What we *don't* claim.** In-cluster Postgres is **not** the production answer. The portability-seam ADR (ka-0000004922) and the Vultr provider survey (ka-0000002019) must answer whether VMPG can support gardenio in production before the implementation epic starts.

A decision item is filed in §6 (D-1) to confirm VMPG/pgvector status before the implementation epic.

### 3.4 NATS

In-cluster, via the gardenio chart's bundled NATS (`nats.enabled: true`). No managed NATS on Vultr; this is the same shape as AWS canonical-prod.

### 3.5 Secrets backend

**No prod-grade secrets backend.** This is part of the cutline (§4). The POC creates K8s Secrets directly via Terraform `kubernetes_secret` resources (or `helm_release` `values`), with operator-supplied tfvars for sensitive values. The External Secrets Operator is installed but has no `ClusterSecretStore` wired up — its presence proves portability of the addon, but the *backend* decision is deferred:

| Candidate | POC fit | Production fit | Status |
|---|---|---|---|
| K8s Secrets (direct) | yes — what the POC does | no | POC-only |
| HashiCorp Vault in-cluster | possible — adds a sibling Helm release | yes, if operator-tolerant | filed as **D-2** in §6 |
| Vultr-side managed secrets | n/a — Vultr does not offer one | — | hard gap; reference in §5 |
| Use AWS Secrets Manager from Vultr | possible — cross-cloud network call | viable, but loses portability claim | filed as **D-2** in §6 |

### 3.6 Gardenio chart Vultr overlay

A new `gardenio/helm/gardenio/values-vultr-poc.yaml` (or, more likely, a `site-deployment-vultr/overlays/vultr-poc/values.yaml`) overrides the AWS-flavored defaults:

| Key | POC value | Why |
|---|---|---|
| `postgres.enabled` | `true` | In-cluster pgvector — see §3.3. |
| `postgres.image.repository` | `pgvector/pgvector` | Vector-enabled Postgres. |
| `nats.enabled` | `true` | In-cluster NATS. |
| `agents.s3.bucket` | `gardenio-sessions-vultr-poc` | Vultr Object Storage bucket. |
| `agents.s3.endpoint` | Vultr Object Storage region endpoint URL | The chart already supports `COOP_S3_ENDPOINT` for MinIO; same mechanism. |
| `agents.s3.region` | Vultr-region equivalent (e.g. `ewr`) | Vultr Object Storage accepts an arbitrary region string for S3-API compat. |
| `ingress.tls.issuer` | `letsencrypt-prod-http01` | Created by the cert-manager Helm release. |
| `ingress.host` | `gardenio.vultr-poc.gardenio.app.acme.example` | Single hostname for POC. |
| (any AWS-LB-Controller annotations) | unset | Vultr LB is provisioned by the in-tree VKE CCM. |
| (any IRSA SA annotations) | unset | Replaced by API-token K8s Secrets where needed. |

The overlay file lives next to the AWS overlay (or in `site-deployment-vultr/` once that exists) — picking the right home is part of the portability-seam ADR (ka-0000004922), not this doc.

---

## 4. Cutline — what is explicitly NOT in the POC

Each line below is a deliberate "no" so the POC can ship on a single afternoon's apply, not on the production timeline. Anything here that we eventually want is a follow-up epic, **not** a stretch goal of the POC.

| Out of scope | Why deferred | Re-opens under |
|---|---|---|
| Multi-region | Vultr VPC 2.0 has no transit-gateway-equivalent; multi-region requires a separate networking design. | A future "Vultr multi-region" epic (does not exist yet — file when implementation epic lands). |
| HA managed-PG (VMPG + read replicas) | pgvector-on-VMPG unconfirmed; HA tuning is production work, not POC work. | Sibling task under the implementation epic, gated on **D-1**. |
| Production-grade secrets backend | No Vultr-native KMS/Secrets Manager; choosing Vault vs AWS-SM-from-Vultr is a real ADR. | **D-2** (§6) → implementation-epic ADR. |
| Migration from `america-e2e-eks` | Owned by Epic 10 (ka-0000001529); POC is **greenfield only**. | Epic 10 remains AWS-internal; a Vultr migration is its own epic if/when needed. |
| Third cloud target (Hetzner, GCP) | The portability seam should make it cheap, but designing for it is a separate epic. | Future epic; not committed. |
| Karpenter-equivalent / spot autoscaling | VKE has fixed node pools; no Karpenter on Vultr. Beyond POC. | Implementation-epic decision agenda. |
| ALB/AWS-LB-Controller install | AWS-only; the POC uses the in-tree Vultr CCM LB. | n/a — design choice. |
| ACM / KMS-equivalent | Replaced by cert-manager + Let's Encrypt; K8s Secrets in lieu of KMS. | **D-2** (§6). |
| ECR-equivalent (private registry on Vultr) | POC pulls public images from `<registry>/gardenio/*`. Vultr Container Registry is a future fork. | Implementation-epic decision agenda. |
| Mezmo / centralized log shipping | POC uses VKE-native pod logs + VictoriaMetrics for metrics only; no log pipeline. | Future Vultr-Observability task, parents Epic 8 (ka-0000007067) if we go production. |
| GitOps (Argo / Flux) | POC uses `helm_release` from Terraform. GitOps is a follow-on. | Future operational-tooling task. |
| CI dispatch of `terraform apply` | Apply runs from an operator's workstation **only**. No CI/CD wiring for the POC. | Implementation-epic decision agenda. |
| acme-specific bleed (account IDs, IAM names, Slack channel IDs) | Module defaults stay portable per umbrella core rule 3. | Lives in the (future) `site-deployment-vultr/` overlay tfvars, never in the module. |
| Vultr account procurement / billing approval | Operator action — captured as a `gb decision` for ops. | **D-5** (§6). |

---

## 5. Known divergences from the AWS design

Each row is a Vultr-vs-AWS divergence that the POC accepts as-is. The accompanying impact doc (ka-0000002100) and resource-map doc (ka-0000007377) catalogue these in full; this list is the **subset the POC has to live with**, not an exhaustive comparison.

| AWS canonical-prod | Vultr POC | Impact on POC |
|---|---|---|
| EKS (managed control plane) | VKE | None — VKE is API-compatible enough for the chart. |
| IRSA / Pod Identity | API-token K8s Secrets, mounted as env vars | Loses fine-grained per-pod IAM. Acceptable for POC; flagged in implementation epic. |
| AWS Load Balancer Controller (`Ingress` → ALB) | In-tree Vultr CCM (`Service type=LoadBalancer` → Vultr LB) | No `Ingress` resources in the POC — gardenio chart's `tls.enabled` NLB path is the model. |
| Route53 (zones managed by external-dns + Terraform) | Vultr DNS (managed by external-dns + Terraform via `vultr_dns_domain`) | Equivalent — but **the parent NS delegation is a manual operator step** the first time around. |
| ACM wildcard cert | cert-manager HTTP-01 per-hostname | No wildcards in POC; one hostname is enough. |
| Secrets Manager | K8s Secrets only | No prod-grade backend — explicit cutline. |
| S3 + KMS-SSE | Vultr Object Storage (S3-API), no KMS | Server-side encryption is Vultr-managed; loses customer-managed-key control. |
| HA NAT per AZ | Single egress (VKE VPC 2.0) | No NAT-cost question on the Vultr side; the POC inherits Vultr's defaults. |
| Karpenter for agent pool | Fixed node pool | Out of scope for POC. |
| CloudWatch logs / metrics | VictoriaMetrics in-cluster only | No managed log retention; pod logs only. |

---

## 6. Open decisions (parked, not blocking the doc)

Filed under epic ka-0000006189 via the divergence-sweep sibling task (ka-0000000366). Each must resolve before the implementation epic starts.

| ID | Question | Who decides | Blocking what |
|---|---|---|---|
| **D-1** | Does Vultr Managed Postgres support pgvector? If no, do we run in-cluster Postgres in production too, or pick a third option? | Sibling research task under the implementation epic, then a `gb decision`. | Production beads DB design on Vultr. |
| **D-2** | Production secrets backend on Vultr — in-cluster Vault, AWS-Secrets-Manager-from-Vultr, or accept "K8s Secrets only"? | Implementation-epic ADR. | external-secrets `ClusterSecretStore` wiring, real secrets at all. |
| **D-3** | NS delegation: who owns the Route53 change to delegate `vultr-poc.gardenio.app.acme.example` to Vultr nameservers? | Matt / DevOps. | First-time apply; one-time step. |
| **D-4** | Vultr Object Storage bucket lifecycle / TF coverage gap (provider survey ka-0000002019 will confirm whether `vultr_object_storage` covers bucket-name + bucket-policy + lifecycle or only the *cluster* of buckets). | Provider-survey task. | Whether bucket creation lives in TF or in a `null_resource` + AWS CLI–style shim. |
| **D-5** | Vultr account procurement: who owns it (acme ops? gardenio ops?), billing approval, org placement, who holds the root API token. | Matt / DevOps. | The first `terraform apply` — cannot start without an account. |
| **D-6** | Object Storage CORS + bucket-policy parity: confirm that `coop peek --archived` and `gb audit` work against Vultr Object Storage with a stock S3 SDK config (mostly `COOP_S3_ENDPOINT` + path-style addressing). | POC-apply step in §7. | POC acceptance — must be answered before the checklist ticks. |
| **D-7** | TLS issuer: HTTP-01 vs DNS-01. POC defaults to HTTP-01 (no provider hookup). If the implementation epic wants wildcards, file a sibling task to add `cert-manager-webhook-vultr`. | Implementation-epic decision agenda. | Wildcards only; not blocking POC. |

The full divergence-sweep (ka-0000000366) will produce more rows; the rows above are the **minimum the POC has to track**.

---

## 7. Acceptance checklist (human ticks each after a real apply)

The POC is "done" when every box below is ticked by a reviewer working from a fresh checkout of the (future) `site-deployment-vultr/overlays/vultr-poc/` (or whatever path the portability-seam ADR settles on). Each item is independently verifiable — no "looks good overall" rows.

### 7.1 Pre-apply preconditions

- [ ] **D-3** resolved: `vultr-poc.gardenio.app.acme.example` NS delegated from `acme.example` (Route53) to Vultr nameservers; `dig +trace NS vultr-poc.gardenio.app.acme.example` shows Vultr's nameservers.
- [ ] **D-5** resolved: a Vultr account exists, billing is approved, and an API token with the necessary scopes is available to the operator.
- [ ] Operator has `VULTR_API_KEY` in env and `vultr` CLI configured (provider sanity check).
- [ ] State backend: operator-local (`local` backend, no remote backend wired for POC).

### 7.2 Apply succeeds

- [ ] `terraform init` succeeds against the pinned Vultr provider version (see ka-0000002019 for the pin).
- [ ] `terraform plan` shows the resource set from §3.1 with no drift and no AWS resources.
- [ ] `terraform apply` completes in ≤ 30 minutes from a cold start (informational target, not a hard gate).
- [ ] `kubectl get nodes` shows 3 Ready nodes in the VKE node pool.
- [ ] No `apt-get`-style manual steps required outside the Terraform run.

### 7.3 Cluster is gardenio-ready

- [ ] `kubectl -n gardenio-system get pods` shows `gardenio-controller`, `coopmux`, `slack-bridge`, `postgres-0`, `nats-0` all `Running 1/1` (or as many replicas as the values file specifies).
- [ ] `kubectl -n gardenio-system exec postgres-0 -- psql -d gardenio -c 'SELECT extversion FROM pg_extension WHERE extname='\''vector'\'';'` returns a non-empty result (pgvector extension is installed).
- [ ] `kubectl -n external-dns logs deploy/external-dns` shows a successful sync to the Vultr DNS zone (no auth or zone-not-found errors).
- [ ] `kubectl -n cert-manager get clusterissuer letsencrypt-prod-http01 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'` returns `True`.
- [ ] `kubectl -n external-secrets get pods` shows the operator `Running`. (No `ClusterSecretStore` expected — cutline.)
- [ ] `kubectl -n victoria-metrics get pods` shows vm-operator + at least one VMSingle + one VMAgent `Running`.

### 7.4 Public reachability

- [ ] `dig +short gardenio.vultr-poc.gardenio.app.acme.example` returns a Vultr LB IP.
- [ ] `curl -sS https://gardenio.vultr-poc.gardenio.app.acme.example/healthz` returns HTTP 200 with a valid Let's Encrypt cert (no `--insecure`, no cert warnings in the browser).
- [ ] The Slack bridge can post to the configured channel: a `gb squawk "vultr-poc reachable"` from inside a test agent pod surfaces in Slack within 30 seconds.

### 7.5 Beads + agent loop

- [ ] `kd create "vultr-poc smoke" --type=task -l project:gardenio-vultr-poc` succeeds against the POC beads endpoint.
- [ ] A test agent pod spawned by the gardenio controller starts, registers with coopmux, reads its session config, and writes session output to Vultr Object Storage (`coop peek --archived <session-id>` returns the transcript via `COOP_S3_ENDPOINT`).
- [ ] **D-6** resolved: the same `coop peek --archived` flow works without setting any Vultr-specific SDK flags beyond `COOP_S3_ENDPOINT` and `COOP_S3_REGION`.

### 7.6 Observability sanity

- [ ] VMSingle has ingested at least one metric from the gardenio namespace (`kubectl port-forward svc/vmsingle 8429` + `curl localhost:8429/api/v1/query?query=up{namespace="gardenio-system"}`).
- [ ] No CrashLoopBackOff pods anywhere in the cluster for 10 consecutive minutes.

### 7.7 Cutline confirmed

- [ ] No multi-region resources exist.
- [ ] No managed-Postgres resource exists.
- [ ] No `ClusterSecretStore`, no Vault, no AWS-SM cross-link.
- [ ] No `aws_*` resources in `terraform state list`.
- [ ] No `helm_release` for the AWS LB controller or ACK controllers.

### 7.8 Teardown

- [ ] `terraform destroy` removes the cluster, LB, DNS zone (or — if zone destroy is risky — explicit `lifecycle.prevent_destroy` on the zone and the operator runs a documented manual NS-undelegation), object-storage bucket, and VPC. No orphaned resources visible in the Vultr console after 1 hour.
- [ ] Estimated cost during the POC lifetime is logged in the bead (ka-0000006197) for the eventual cost-delta doc (Epic 11).

### 7.9 Hand-off

- [ ] Operator opens a `gb decision` on ka-0000006189: "Vultr POC ready to leave research and enter implementation." A human reviewer ticks `approve` → the implementation epic gets filed as a sibling to ka-0000006189.
- [ ] This document is updated with the actual hostname, the actual provider version pin, the actual node-pool sizing, and any deviations from §3 — turning it from a plan into a runbook.

---

## 8. What this doc does NOT cover (and where to look)

- **The provider/resource map.** Lives in `docs/research/canonical-prod-vultr-provider-map.md` (ka-0000007377). This POC only references the subset of resources it uses.
- **The portability seam.** Whether the module is one composition root, an in-module abstraction, or two parallel modules is the ADR under ka-0000004922. The POC layout will follow that ADR's call — until then, the `site-deployment-vultr/overlays/vultr-poc/` path is a working name, not a commitment.
- **The per-epic impact analysis.** Lives in `docs/research/canonical-prod-vultr-architecture-impact.md` (ka-0000002100). This POC consumes its conclusions in §5 + §6; it does not re-derive them.
- **The Vultr provider survey.** Pinned versions, missing resources, credentials — lives in the ka-0000002019 task output. The POC's `terraform { required_providers { vultr = { … } } }` block will pin to whatever that doc lands on.
- **The umbrella roadmap.** `gardenio/plans/canonical-prod-round2-refinement.md` is the canonical 12-epic plan; nothing here changes that plan, only adds a Vultr-portability vertical to it.

---

## 9. Sign-off

This plan is **research output**, not a green light. The implementation epic (sibling to ka-0000006189, to be filed after a human resolves the parent epic's done-when `gb decision`) is the artifact that authorizes a real apply. Until then: no Vultr resources, no API tokens in CI, no `feat/canonical-prod-vultr/*` branches in `site-deployment`.

> Per umbrella core rule 4: any future migration from `america-e2e-eks` to Vultr is owned by Epic 10 (ka-0000001529) and is **not** part of this POC.
