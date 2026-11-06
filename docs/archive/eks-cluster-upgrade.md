# EKS cluster upgrade runbook (control plane + managed addons + nodes)

Operator runbook for hopping a gardenio EKS cluster to a newer Kubernetes
minor. Covers `poc-gardenio-eks` (staging, account `123456789012`) and
`prod-gardenio-eks` (canonical prod, account `123456789012` — same account as
staging, distinct cluster; post-P4.4 cutover ka-0000006191) — both stand on the same
`gardenio/terraform/modules/eks` module and the same
`composition/{poc,canonical-prod}` roots, so the procedure is identical;
only the tfvars / credentials differ.

> **Cadence:** aggressive-to-1.36 (decision `ka-0000001451`). One minor per
> hop, each behind its own operator `gb decision` apply gate.
> **Prior art:** per-hop artifacts under
> `gardenio/terraform/composition/poc/artifacts/eks-upgrade-hop*.md`.
> **Design source of truth:** `docs/research/canonical-prod-eks-addon-pinning.md`.

---

## THE INVARIANT — read this first

**Managed-addon versions are terraform-owned. NEVER bump an addon with a
bare `aws eks update-addon`.**

`modules/eks` pins every core addon (`vpc-cni`, `kube-proxy`, `coredns`,
`aws-ebs-csi-driver`) to a version resolved from `local.eks_addon_defaults`
keyed by the cluster's Kubernetes minor (`var.cluster_version`). The
`"latest"` entries in `var.cluster_addons` resolve to that pinned table
value — they do **not** leave `addon_version` null.

### Why (bug `ka-0000003453`)

A bare `aws eks update-addon --addon-name aws-ebs-csi-driver
--addon-version <v> --resolve-conflicts PRESERVE` (no
`--service-account-role-arn`) re-renders the `kube-system/ebs-csi-controller-sa`
ServiceAccount **without** its `eks.amazonaws.com/role-arn` annotation. The
EKS IRSA/pod-identity webhook then injects no `AWS_ROLE_ARN` /
`AWS_WEB_IDENTITY_TOKEN_FILE` / token volume into the new controller pods;
the AWS SDK falls through to IMDS, finds no role, and the
`ebs-csi-controller` pods **CrashLoopBackOff** (observed ~15 min during
hop-1, addon stuck `UPDATING`). Only `aws-ebs-csi-driver` is IRSA-bound in
this module (`local.addon_service_account_role_arns`), so it is the sole
addon that hits this failure mode — but it is a hard failure.

Driving the bump through `terraform apply` instead always re-emits
`UpdateAddon` **with** `service_account_role_arn` set (the module wires it
on every `aws_eks_addon`), which deterministically (re)stamps the SA
annotation. That is the durable fix: terraform owning the version means the
annotation-dropping CLI path is never the upgrade path.

---

## Per-hop procedure

Run from the composition root (`gardenio/terraform/composition/poc` or
`.../canonical-prod`). Credential paths:

- **POC (`123456789012`):** AKIA IAM user `arn:aws:iam::123456789012:user/gardenio`
  from SM `gardenio/customer-poc-aws-creds` (advice `ka-0000007102`); STS not
  viable in this account. Also reachable from an agent pod via the
  `infra-gardener-canonical-creds` k8s Secret in the `gardenio` namespace.
- **Prod (`123456789012`):** the composition's own credential path — IAM user
  `gardenio` on acct `123456789012` (see `composition/canonical-prod/SECRETS.md`).
  (Post-P4.4 ka-0000006191; do NOT auth into the frozen `234567890123` — that is
  the old america-e2e rollback source, no longer the prod cluster's account.)

### 1. Pre-flight ground truth (read-only)

```sh
CLUSTER=poc-gardenio-eks   # or prod-gardenio-eks
aws eks describe-cluster --name "$CLUSTER" --query 'cluster.version' --output text
for a in kube-proxy vpc-cni coredns aws-ebs-csi-driver; do
  echo -n "$a => "
  aws eks describe-addon --cluster-name "$CLUSTER" --addon-name "$a" \
    --query 'addon.[addonVersion,serviceAccountRoleArn]' --output text
done
# UPGRADE_READINESS insights should be ALL PASSING for the target minor,
# and `apiserver_requested_deprecated_apis` counters ZERO, before you hop.
```

### 2. Resolve the target minor's addon default versions

```sh
NEXT=1.32   # target minor
for a in kube-proxy vpc-cni coredns aws-ebs-csi-driver; do
  echo -n "$a => "
  aws eks describe-addon-versions --addon-name "$a" --kubernetes-version "$NEXT" \
    --query "addons[0].addonVersions[?compatibilities[0].defaultVersion==\`true\`].addonVersion | [0]" \
    --output text
done
```

Confirm `modules/eks/main.tf` `local.eks_addon_defaults` has a row for
`$NEXT`. If it is missing, or the published defaults have moved, **update
the table row in a code MR first** (see §3) — do not paper over it with a
CLI update.

### 3. Bump the version in terraform (code MR)

1. Bump `eks_version` in the composition tfvars (and/or `var.eks_version`
   default) to `$NEXT`. This flows to `module.eks` as `var.cluster_version`.
2. Ensure `local.eks_addon_defaults["$NEXT"]` exists in
   `modules/eks/main.tf` and matches the versions resolved in §2. Add or
   correct the row, re-verified from the addon API — a stale row (esp. the
   coredns minor-family jump at 1.33: `v1.11.4` → `v1.12.4`) can force an
   unintended addon change.
3. `terraform fmt -recursive && terraform validate` — both clean.
4. Open the MR, link the hop task/decision bead, get it merged.

You do **not** need to touch `var.cluster_addons` in the compositions —
they inherit the module default map, which self-pins off
`var.cluster_version` via the table.

### 4. Plan behind the operator apply gate

> **`-var confirm_eks_version_change=true` is MANDATORY on canonical-prod**
> (bug ka-0000006610). Without it the plan below is a **NO-OP on the version**
> — `module.eks` deliberately reconciles toward the LIVE minor, so you will
> see "No changes" and conclude the hop is already done when it has not
> started. If you see no version change, check this flag first.
>
> Why the gate exists: the routine release apply is `-target=module.gardenio`
> (`.rwx/deploy.yml` deploy-prod), and `-target` keeps the targeted objects
> **and everything they depend on**. `module.gardenio` used to depend on
> `module.eks.aws_eks_cluster.this` transitively — `providers.tf` configured
> the helm/kubernetes providers from `module.eks.cluster_endpoint` /
> `cluster_ca_data`, and `dns.tf`/`platform.tf` consumed `module.eks.oidc_*`,
> all of which `modules/eks/outputs.tf` resolved off the cluster resource.
> Measured with `terraform graph`, the cluster was 1 of the 89 nodes reachable
> from the `module.gardenio.*` seeds. So before the gate, merging a rung ahead of
> its apply armed the next release deploy to upgrade the control plane
> unattended and leave kubelet + every addon a minor behind.
>
> **That coupling is gone on canonical-prod (bug ka-0000007293):**
> `eks_identity.tf` reads the cluster's identity attributes back through
> `data.aws_eks_cluster.identity`, and the whole control plane now measures
> OUTSIDE a 79-node release closure. Re-measure rather than trust these numbers
> — see release-pipeline.md "What the `-target=module.gardenio` closure actually
> contains" for the one-command recipe and the CI gate that pins it.
>
> **The gate stays mandatory anyway, and this runbook is exactly why.** Target
> pruning protects the RELEASE path only. The command below is a deliberate
> `-target=module.eks` apply, which reaches `version` directly — the gate is
> what keeps *that* apply (and any full apply) from moving the control plane
> unless you asked it to. It also still protects `composition/poc`, which keeps
> the direct wiring. With the gate, `eks_version` in `prod-real.tfvars` is a
> *desired pin* that is safe to merge early, and this flag is the only thing
> that moves the cluster.
>
> A held-back rung is never silent: every plan emits the
> `cluster_version_change_is_gated` **warning** (a `check` block — a warning,
> never an error, so a merged rung cannot brick the release lane), and the
> read-only drift alarm prints a `PENDING GATED EKS HOP:` line.
>
> The gate covers `version` **only**. Every other `aws_eks_cluster` argument
> (log types, `access_config`, public-access CIDRs, `encryption_config`, …) is
> no longer applied by a release deploy either — but that means it is now
> **inert** until someone runs a targeted apply, not that it is safe to merge
> and forget. The drift alarm still classifies it (`INERT DRIFT`,
> `scripts/classify-release-closure-drift.py`). Merge a cluster-argument change
> only alongside the apply you intend for it.

```sh
terraform init -backend-config=backend-<env>.hcl
terraform plan -target=module.eks \
  -var confirm_eks_version_change=true \
  -var-file=<live-matching.tfvars> -out=hop.tfplan
```

**Expect:**
- **0 destroy.** The addon resources reconcile in place. If a plan shows an
  addon being destroyed/recreated, STOP — a `vpc-cni`/`kube-proxy` recreate
  is a pod-network blip (addon-pinning doc §5.4).
- `aws_eks_cluster.this` `version` changes to `$NEXT` (the real,
  irreversible control-plane change).
- Each `aws_eks_addon.*` `addon_version` changes to the `$NEXT` table
  value; `service_account_role_arn` is present on `aws-ebs-csi-driver`
  (never null). This is the property that re-stamps the SA annotation.

- `aws_eks_node_group.this["infra"]` `version` changes to `$NEXT` too. The
  nodegroup reads the SAME gated version local as the control plane, so the
  confirmed hop rolls both in one apply and they can never be gated apart
  (audit F8 ka-0000005162). Step 5.2 below is the live-node roll, not a
  version-ownership fix.

The targeted `-target=module.eks` keeps the apply to the operator-gated
change and off unrelated accumulated drift (currency chart bumps, IAM
drift, route53 records) — mirror the hop-1 artifact.

**One rung per apply.** EKS `UpdateClusterVersion` rejects a skipped minor
(1.31 -> 1.33 is refused), so never stack two rungs into one confirmed apply
— see advice ka-0000001590.

### 5. Apply, then roll nodes

1. `terraform apply hop.tfplan` — control plane + addons + the managed
   nodegroup's terraform-owned `version` reconcile. (Applying a saved plan
   file carries the confirmed `-var` from step 4; you do not repeat it.)
2. Roll the managed nodegroup:
   `aws eks update-nodegroup-version --cluster-name "$CLUSTER"
   --nodegroup-name infra --kubernetes-version "$NEXT"` (rolls one node at a
   time, `maxUnavailable=1`, honors PDBs: coredns, ebs-csi-controller).
3. Karpenter nodes drift-replace on their own once the control plane is on
   `$NEXT` (EC2NodeClass `al2023@latest`); older nodes are marked
   `Drifted=True` and replaced under the disruption budget — no manual step.

### 6. Post-upgrade verification

```sh
# addons ACTIVE on target versions, ebs-csi SA role restored
kubectl get sa -n kube-system ebs-csi-controller-sa \
  -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}{"\n"}'   # must be non-empty
kubectl get pods -n kube-system -l app=ebs-csi-controller                      # all Running/Ready, 0 restarts
```

- Control plane on `$NEXT`, health issues none.
- All addons ACTIVE on the `$NEXT` table versions.
- EBS provisioning smoke: create a `gp3` PVC → Bound; consumer pod writes +
  reads a file; confirms ebs-csi IRSA end-to-end.
- Fresh UPGRADE_READINESS insights ALL PASSING (incl. "Deprecated APIs
  removed in <next+1>") to clear the runway for the following hop.

Compare `terraform output -json addon_versions` (module output) against the
live `describe-addon` versions — they must match, proving terraform owns
the version.

---

## Emergency-only CLI fallback

If a CLI `update-addon` is ever genuinely unavoidable (terraform apply
blocked, urgent CVE), it **MUST** thread the IRSA role ARN so the SA
annotation is not dropped:

```sh
EBS_ROLE_ARN=$(aws eks describe-addon --cluster-name "$CLUSTER" \
  --addon-name aws-ebs-csi-driver --query 'addon.serviceAccountRoleArn' --output text)
# If that returned "None" (already dropped), read it from IAM instead:
#   arn:aws:iam::<acct>:role/gardenio/<cluster>-ebs-csi-irsa

gardenio/scripts/eks-update-addon.sh "$CLUSTER" aws-ebs-csi-driver v1.62.0-eksbuild.1
```

`gardenio/scripts/eks-update-addon.sh` is the guardrailed wrapper: it
auto-resolves and always passes `--service-account-role-arn` for
IRSA-bound addons, refusing to run bare. After any CLI update, **reconcile
terraform state with a follow-up `terraform apply`** so state and the live
cluster do not silently diverge on version.

If the annotation was already dropped and pods are CrashLooping, the
immediate un-stick is:

```sh
kubectl annotate sa -n kube-system ebs-csi-controller-sa \
  eks.amazonaws.com/role-arn="$EBS_ROLE_ARN" --overwrite
kubectl delete pod -n kube-system -l app=ebs-csi-controller   # let them respawn with IRSA
```

then bring state back in line via terraform.
