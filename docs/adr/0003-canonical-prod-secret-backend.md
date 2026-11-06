# ADR 0003 — canonical-prod (Vultr): secret backend path — sealed-secrets POC → in-cluster Vault HA prod

> Status: **Accepted** (ratifies SB-V-2; see §8)
> Bead: ka-0000006424 (Ratify SB-V-2: secret backend path)
> Parent epic: ka-0000008037 (Epic 5 — customer-poc stateful services: RDS, S3, Secrets Manager)
> Umbrella epic: ka-0000005972 (canonical-prod, research-only)
> Source research: `docs/research/canonical-prod-vultr-stateful.md` §4 (`SB-V-2`, §4.8 "the `gb decision` to file") — not in this repo, the canonical-prod research tree was excluded from the 2026-09-16 export
> Prior decision that spawned this task: `gb decision` ka-0000007755 (choice: `all-three` → SB-V-2 / PG-V-3 / DR-V-1 filed as tasks under ka-0000008037)
> Prior art:
> - [`docs/adr/0002-canonical-prod-multi-cloud-portability-seam.md`](0002-canonical-prod-multi-cloud-portability-seam.md) — the module seam this backend plugs into (`modules/vultr-data/` owns the secret store; `modules/platform/` owns ESO)
> - `docs/research/canonical-prod-secrets-manager.md` — the AWS-SM path convention + secret inventory this ADR preserves (not in this repo, see note above)
> - `docs/research/canonical-prod-vultr-provider-map.md` §3.2–3.3 — the provider-map conclusion ("Vault, for k8s-auth scoping parity with IRSA") (not in this repo, see note above)
> Siblings under ka-0000008037 (filed together by ka-0000007755):
> - PG-V-3 — Vultr Managed PG pgvector smoke test
> - DR-V-1 — looser RPO/RTO budget acceptance

---

## 1. Context

The gardenio canonical-prod design runs on AWS today: app secrets live in **AWS
Secrets Manager**, are scoped per-component via **IRSA**, and reach workloads
through **External Secrets Operator (ESO)** `ClusterSecretStore` →
`ExternalSecret` → K8s `Secret`. The Vultr canonical-prod track (umbrella
ka-0000005972) removes the AWS dependency. Neither AWS Secrets Manager nor IRSA
ports to Vultr, so the secret backend is an open fork every downstream ESO/chart
change depends on.

The stateful-services research doc (ka-0000002671, MR !2464) enumerated five
candidates plus a defer option and recommended a **two-phase path**:

- **POC**: **sealed-secrets** (Bitnami) — sealed YAML committed to git,
  decrypted in-cluster by the sealed-secrets controller. No ESO, no external
  runtime dependency.
- **Production**: **in-cluster HashiCorp Vault HA** — k8s-auth gives per-SA
  scoping at parity with IRSA; ESO's `vault` backend preserves the existing
  path shape.

This ADR **ratifies** that recommendation as SB-V-2, and — because SB-V-2's
whole reason to exist is to *unblock chart/ESO work in the implementation epic*
— it also pins the concrete wiring: which knob flips per backend, and what must
NOT change in app code or the chart's rendered surface.

This ADR decides **only the secret backend and its wiring seam**. It does not:
own the migration *execution* (SB-V-3, a follow-up plan); decide where Vault's
unseal keys live beyond naming the constraint (SB-V-4); or design the audit sink
plumbing (SB-V-5). Those are named in §7 as follow-ups.

### 1.1 What the chart already gives us (the seam is already built)

The gardenio Helm chart does **not** hard-code AWS Secrets Manager. Every
`ExternalSecret` template references the store indirectly:

```yaml
# gardenio/helm/gardenio/templates/external-secrets.yaml (representative)
  secretStoreRef:
    kind: {{ .Values.externalSecrets.secretStoreKind | default "ClusterSecretStore" }}
    name: {{ .Values.externalSecrets.secretStoreName }}
  target:
    name: <stable K8s Secret name>          # what app pods mount — never changes
  data:
    - secretKey: token
      remoteRef:
        key: {{ .Values.externalSecrets.beadsToken.remoteRef | quote }}   # gardenio/<env>/<component>/<name>
```

Two facts follow, and they are the backbone of this decision:

1. **The store is a value, not a constant.** `externalSecrets.secretStoreName` /
   `secretStoreKind` (`values.yaml` ~L2430) select the `ClusterSecretStore`.
   Today the AWS overlays set it to an AWS-SM-backed store
   (`secretStoreName: aws-secrets-manager` in
   `deploy-values/gardenio-rwx-prod.yaml`; `aws-secretsmanager` in
   `gardenio-rwx.yaml`). Repointing to a Vault-backed store is a **deploy-values
   edit plus one new `ClusterSecretStore` object** — not a template change.
2. **The app-facing interface is the `target.name` K8s Secret + its keys**, not
   the backend. `remoteRef.key` is already the portable
   `gardenio/<env>/<component>/<name>` path (e.g.
   `gardenio/prod/postgres-password`, `gardenio/prod/nats-agent-token` in
   `gardenio-rwx-prod.yaml`). App Deployments mount `target.name`; they never
   name the backend.

The research doc's §4.9 claim — "switch backends by changing
`ClusterSecretStore.spec.provider`, no per-secret churn" — is therefore already
true of the chart as it stands. This ADR's job is to ratify the choice and
record exactly how each phase engages that seam.

---

## 2. Decision

### 2.1 Ratified path

| Phase | Backend | Engages the seam by |
|---|---|---|
| **POC** (Vultr, [`gardenio/plans/canonical-prod-vultr-poc.md`](../../gardenio/plans/canonical-prod-vultr-poc.md)) | **sealed-secrets** | **Bypasses ESO.** Sealed YAML decrypts *directly* into a K8s `Secret` whose name + keys equal the ExternalSecret `target`. `externalSecrets.enabled=false`. |
| **Production** (Vultr canonical-prod) | **in-cluster Vault HA** (3 replicas, Raft on Vultr Block Storage, auto-unseal via Vultr Object Storage transit) | **Keeps ESO.** New `ClusterSecretStore` with `provider: vault`; `externalSecrets.secretStoreName` repointed; `remoteRef.key` paths unchanged modulo the KV-v2 mount prefix. |

This supersedes, for any Vultr deployment aiming at production reliability, the
POC plan's interim "K8s Secrets straight from Terraform" (`kubernetes_secret`)
default. `kubernetes_secret` remains acceptable only for throwaway ephemeral
smoke rigs; sealed-secrets is the first artefact-that-survives step, and Vault is
the production target.

### 2.2 POC wiring — sealed-secrets (ESO bypassed)

sealed-secrets is **not** an ESO backend. Its controller consumes a
`SealedSecret` CR (safe to commit — encrypted with the controller's public key)
and emits a plain `Secret`. To keep app pods unchanged, the seam is honoured at
the **produced-Secret** level, not the store level:

- The `SealedSecret` MUST decrypt to a `Secret` whose **`metadata.name` and data
  keys exactly match the `target.name` / `target.template.data` an
  `ExternalSecret` would have produced** (e.g. Secret `gardenio-beads-token` with
  key `token`; Secret `postgres` with key `password`). App Deployments are
  identical whether ESO or sealed-secrets created the Secret.
- `externalSecrets.enabled=false` in the POC overlay so the chart renders **no**
  `ExternalSecret`/`ClusterSecretStore`; the postgres-credentials break-glass
  path (`templates/postgres/credentials.yaml`, gated on `not
  externalSecrets.enabled`) already anticipates this shape.
- **Path shape is preserved as a naming convention, not a live lookup.** The
  logical `gardenio/<env>/<component>/<name>` maps to the Secret name via
  `/`→`-` (research §4.9): `gardenio/prod/slack-bridge/bot-credentials`
  → Secret `gardenio-prod-slack-bridge-bot-credentials`. Sealed YAML is filed on
  disk under `secrets/<env>/<component>/<name>.sealed.yaml` so the git tree
  mirrors the path.
- **Seeding**: an operator runs `kubeseal` against the target cluster's
  controller public key for each secret in the inventory (§4), commits the
  sealed YAML, and the chart/GitOps applies it. No plaintext ever enters git.

### 2.3 Production wiring — Vault + ESO (seam flip only)

At the production cutover, ESO comes back and the chart returns to its native
shape. The change set is deliberately small:

1. **New `ClusterSecretStore`** (owned by `modules/platform/` per ADR 0002, or a
   platform add-on chart) with:
   ```yaml
   spec:
     provider:
       vault:
         server: "https://vault.gardenio-system.svc:8200"
         path: "secret"            # KV-v2 mount
         version: "v2"
         auth:
           kubernetes:
             mountPath: "kubernetes"
             role: "gardenio-<component>"   # SA → Vault policy → path scoping
             serviceAccountRef:
               name: <component-sa>
   ```
2. **Repoint the store**: `externalSecrets.secretStoreName: vault-backend`
   (`secretStoreKind: ClusterSecretStore`) in the Vultr prod deploy-values.
   `externalSecrets.enabled=true`.
3. **`remoteRef.key` paths are preserved** as the same logical
   `gardenio/<env>/<component>/<name>`. The **only** delta is the KV-v2 physical
   read path: ESO's vault provider reads `secret/data/<path>`. This is handled
   at the store/provider level (mount = `secret`, version `v2`), so the
   per-secret `remoteRef.key` values in deploy-values stay byte-for-byte
   identical to the AWS-SM ones. **No per-secret ExternalSecret churn.**
4. **Per-component scoping** returns to IRSA parity: each component's
   ServiceAccount maps through a Vault k8s-auth role to a policy that grants read
   on exactly its `gardenio/<env>/<component>/*` prefix — the direct analogue of
   the per-component IRSA role in the AWS design.

### 2.4 The invariant this ADR protects

**App code and app Deployment specs never change across all three states**
(AWS-SM today, sealed-secrets POC, Vault prod). The only things that move are:
(a) whether ESO is enabled, (b) the `ClusterSecretStore` provider block, and (c)
`externalSecrets.secretStoreName`. Everything a workload sees — the mounted
Secret name and its keys — is invariant. Any implementation-epic change that
would force an app-Deployment edit to swap backends is a **violation of this ADR**
and should be rejected in review.

---

## 3. Alternatives considered (and why they lost)

Full pro/con tables are in research §4.2–4.6; condensed rationale here.

### 3.1 AWS Secrets Manager, cross-cloud — rejected as the target
Zero path-migration cost, but pulls AWS back into the dependency graph and
defeats the entire "leave AWS" thesis of the Vultr track. It also needs a
long-lived AWS IAM access key living in a Vultr-side K8s Secret — a bootstrap
secret with no clean rotation story. **Kept only as a named fallback** for a
time-boxed transition window (e.g. "Vultr compute but AWS-SM for N months");
never the long-term answer.

### 3.2 Bitwarden Secrets Manager — rejected
Lowest in-cluster ops burden (SaaS) and it has an ESO backend with
project-scoped tokens, but it trades a self-hosted control plane for vendor
lock-in to a *different* SaaS, moves the audit trail out of the gardenio ops loop,
and still carries a long-lived access token in K8s. Reasonable only if the org
already standardises on Bitwarden — gardenio does not.

### 3.3 SOPS (encrypted-in-git) — rejected
No runtime dependency and gitops-friendly, but it inherits sealed-secrets'
file-scope (not per-SA) problem *and* adds per-change workflow friction
(edit → decrypt → re-encrypt → commit). Its decryption keys have to live
somewhere; if that's a cloud KMS we're back to a cloud dependency. Acceptable
only for very low secret-churn, gitops-native teams — not the gardenio operating
model.

### 3.4 Vault as the POC backend too — rejected for POC, chosen for prod
Vault is the production answer, but standing up an HA Vault (Raft quorum,
unseal-key custody, upgrade train) purely to prove Vultr portability is
over-provisioning. sealed-secrets proves the portability thesis at near-zero
infra cost; Vault arrives when per-SA scoping becomes a hard requirement, i.e.
production.

### 3.5 `defer` (stay on `kubernetes_secret`) — rejected as an endpoint
Fine as the *interim* inside the POC (plaintext Secrets from Terraform for an
ephemeral rig), but it is not an artefact that should survive the POC:
plaintext-in-tfstate is not a production secret posture. sealed-secrets is the
minimal step up that produces something git-committable and reviewable.

---

## 4. Secret inventory carried forward (unchanged, two deltas)

The inventory in `canonical-prod-secrets-manager.md` §4 ports unchanged — same
`gardenio/<env>/<component>/<name>` paths, same owners, same rotation cadences,
across all three backends (research §4.10). Two Vultr-specific deltas:

1. **`gardenio/<env>/rds/master-password`** — on Vultr, re-rolled by `terraform
   apply` (no managed rotation Lambda). Its inventory "Source" column becomes
   "TF + in-cluster rotator CronJob (manual schedule)". Note the chart already
   models a fast-refresh `rotated-db` category (`values.yaml` ~L2443) precisely
   because a backend-side rotation must reach the daemon before it CrashLoops on
   a stale password (the P0 ka-0000006811 shape) — that refresh discipline is
   backend-agnostic and survives the swap.
2. **`gardenio/<env>/vultr/api-key`** (new) — Vultr API key for external-dns +
   the in-cluster snapshot/restore CronJob. Owner: ops. Rotation: 90d manual.

All other paths (slack-bridge, gitlab-bridge, jira-bridge, agents,
observability, registry, NATS) port without change — verifiable against
`deploy-values/gardenio-rwx-prod.yaml`, which already uses these exact paths.

---

## 5. Consequences

### 5.1 Immediate — unblocks the implementation epic
The ExternalSecret/ClusterSecretStore shape is now known well enough to draft the
Helm overlay changes:
- **POC overlay** (Vultr): `externalSecrets.enabled=false` + a set of
  `SealedSecret` CRs whose produced Secret names/keys match the ExternalSecret
  targets. A `secrets/<env>/<component>/<name>.sealed.yaml` tree.
- **Prod overlay** (Vultr): a `vault` `ClusterSecretStore` +
  `externalSecrets.secretStoreName: vault-backend` + `enabled=true`;
  `remoteRef.key` values copied verbatim from `gardenio-rwx-prod.yaml`.
- **Vault infra**: goes in `modules/vultr-data/` (the store) with the
  `ClusterSecretStore` add-on in `modules/platform/` (ESO is already a platform
  add-on, ADR 0002 §2.1). Raft on Vultr Block Storage; auto-unseal via Vultr
  Object Storage transit.

### 5.2 Blockers / risks named (not resolved here)
- **sealed-secrets key custody (SB-V-4-adjacent).** The controller's private key
  IS the entire keyring — losing it makes every sealed secret unrecoverable. The
  POC must back up the controller's sealing key off-cluster from day one
  (operator-managed, e.g. 1Password), and document key-rotation (re-seal all on
  key roll). This is a **precondition** for using sealed-secrets beyond a
  throwaway rig.
- **Vault unseal/recovery-key custody (SB-V-4).** Vault's own credentials cannot
  live in Vault. They live operator-managed offline (1Password vault or
  equivalent), cross-referencing `canonical-prod-vultr-provider-map.md` §3.2
  open question 2. This is the classic bootstrap problem and must be solved
  before the prod cutover, not during it.
- **Vault HA on Vultr.** 3-replica Raft needs stable Block Storage and a working
  auto-unseal transit path; if auto-unseal is unavailable, every Vault pod
  restart needs a manual unseal quorum — unacceptable for prod. Auto-unseal
  viability on Vultr is a **prod-gate item** for the implementation epic.
- **Audit sink (SB-V-5).** Vault's native audit log ships to Loki via in-cluster
  Vector — same sink as workload logs. Wiring is a follow-up, not this ADR.

### 5.3 Migration sequencing (SB-V-3 — follow-up plan, not executed here)
The POC→prod cutover is milestone-level, owned by a follow-up plan under
ka-0000008037:
1. Stand up Vault HA + auto-unseal; verify unseal survives a full quorum
   restart.
2. Seed every inventory path into Vault KV-v2 at `secret/gardenio/<env>/...`.
3. Create the `vault` `ClusterSecretStore` + per-component k8s-auth roles/policies.
4. Flip `externalSecrets.enabled=true` + `secretStoreName: vault-backend` in a
   canary env; confirm every `target` Secret materialises identically.
5. Roll to prod; retire the sealed-secrets controller + sealed YAML once Vault
   is authoritative.
Because the seam is `secretStoreName` + provider block only, steps 3–5 are a
deploy-values change plus one CR — no app redeploys for the switch itself.

### 5.4 Out of scope (explicitly)
- The migration plan's execution + timing (SB-V-3).
- Final unseal-key custody mechanism (SB-V-4) — this ADR fixes the *constraint*
  (offline, operator-managed, outside Vault), not the tool.
- Vault audit-pipeline plumbing (SB-V-5) — sink named (Loki via Vector), wiring
  deferred.
- Any AWS-side change. The AWS canonical-prod path keeps AWS-SM + IRSA
  unchanged; this ADR governs the Vultr track only.

---

## 6. How we will know this was the right call (falsification criteria)

1. **Zero app-Deployment diffs across the backend swap.** If switching
   AWS-SM → sealed-secrets → Vault forces an edit to any app pod spec (mounts,
   env, Secret names), the invariant in §2.4 failed and the seam is wrong.
2. **The prod switch is a deploy-values + one-CR change.** If flipping to Vault
   needs per-secret `remoteRef` edits, §2.3's "no per-secret churn" claim was
   false and the KV-v2 mount handling needs rework.
3. **sealed-secrets produces byte-identical Secret names/keys to the ESO
   targets.** Verified by rendering the POC overlay and diffing the produced
   Secret objects against the AWS overlay's ExternalSecret `target`s.
4. **Vault k8s-auth reaches per-component parity with IRSA.** Each SA reads only
   its own `gardenio/<env>/<component>/*` prefix and nothing else — provable with
   a deny-test from a foreign SA.
5. **No plaintext secret ever lands in git** in the sealed-secrets POC. A git
   history scan / secret-scanner CI gate stays clean.

If (1) or (2) fail within the first Vultr consumer, revisit — most likely by
formalising a produced-Secret contract test in the chart's test suite.

---

## 7. Open follow-ups (not in this ADR)

| Ref | Item | Disposition |
|---|---|---|
| SB-V-3 | POC sealed-secrets → prod Vault migration sequencing | Follow-up plan under ka-0000008037; §5.3 sketches it |
| SB-V-4 | Where Vault's unseal/recovery keys live | Constraint fixed here (offline, operator-managed); mechanism TBD |
| SB-V-5 | Vault audit-log sink | Named (Loki via in-cluster Vector); wiring deferred |
| — | sealed-secrets sealing-key backup + rotation runbook | Precondition for POC; document in `gardenio/docs/secret-rotation.md` |
| — | Produced-Secret contract test (sealed-secrets Secret name/keys == ESO target) | Chart test-suite follow-up in the implementation epic |
| — | Vultr auto-unseal transit viability spike | Prod-gate item; blocks the prod cutover if unavailable |

---

## 8. Ratification

This ADR **ratifies SB-V-2**: the Vultr canonical-prod secret backend is
**sealed-secrets for the POC and in-cluster Vault HA for production**, engaging
the chart's existing `ClusterSecretStore` seam so that app code and app
Deployment specs are invariant across the AWS-SM → sealed-secrets → Vault
progression (§2.4).

It is the artefact form of the `gb decision` scoped in research §4.8; per that
section each option's `artifact_type` is `plan` (a backend deployment plan), and
this ADR is that plan for the recommended option. Consequences:

- The implementation epic (child of ka-0000008037) consumes §2.2–2.3 as the
  fixed wiring for the POC and prod Helm overlays.
- SB-V-3 (migration sequencing) consumes §5.3 as its skeleton.
- SB-V-4 / SB-V-5 remain open with the constraints in §5.2 fixed.
- The `gardenio/<env>/<component>/<name>` path convention from
  `canonical-prod-secrets-manager.md` §3 is confirmed stable across every
  candidate (research §4.9) and is the interface the implementation epic builds
  against.

Outcome linked back to ka-0000008037 (parent epic) and ka-0000002671 (source
research). If the operator prefers a different production backend than Vault, the
alternatives in §3 carry the pro/con needed to re-decide without re-doing the
seam analysis — only §2.3's provider block changes.
