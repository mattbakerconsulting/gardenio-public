# Enable the e2e-coverage EFS store (Path A)

Operator runbook for turning on the ReadWriteMany EFS filesystem the
e2e-coverage epic (ka-0000006199) needs. Background: bug **ka-0000001979**,
decision **ka-0000006788** (chosen: **efs / Path A**).

## Why

The coverage PVC (`gardenio/helm/gardenio/templates/coverage.yaml`) is
`accessModes: [ReadWriteMany]` — 13 daemon Deployments plus dynamic agent pods
mount it concurrently across Karpenter nodes, and a drain helper pod tars it
afterward. EBS block storage is RWO-only, and `prod-gardenio-eks` ships **only**
the `aws-ebs-csi-driver`. So the `coverage=1` deploy-rwx PVC never binds
("Volume capabilities not supported"). This provisions EFS + the
`aws-efs-csi-driver` addon + an RWM StorageClass so the PVC binds.

The code is merged **default-off**: nothing changes on prod until the operator
sets the gate and applies. Root-level resources here are **outside** the
routine `terraform apply -target=module.gardenio` deploy closure, so enabling is
a deliberate full apply — NOT a per-release deploy.

## What the gate controls (all in `composition/canonical-prod`)

`coverage_efs_enabled = true` turns on, together:

1. **`module.efs`** (`modules/efs`, `coverage_efs.tf`) — an encrypted EFS
   filesystem, one mount target per node (private-app) subnet, and a security
   group allowing NFS/2049 from the cluster-managed SG.
2. **`module.eks`** (`main.tf`) — the `efs-csi` IRSA role
   (`efs-csi-controller-sa`, `AmazonEFSCSIDriverPolicy`) **and** the
   `aws-efs-csi-driver` managed addon, pinned to
   `var.coverage_efs_csi_addon_version`.
3. **`kubernetes_storage_class.coverage_efs`** (`coverage_efs.tf`) — the
   cluster-scoped `efs.csi.aws.com` StorageClass named
   `var.coverage_efs_storage_class_name` (default `gardenio-efs-coverage`),
   `provisioningMode: efs-ap`, `fileSystemId` = the new filesystem.

## Steps

1. **Resolve the addon version pin** (the `validation{}` on
   `coverage_efs_csi_addon_version` refuses an empty value when the gate is on):

   ```bash
   aws eks describe-addon-versions --addon-name aws-efs-csi-driver \
     --kubernetes-version 1.31 \
     --query 'addons[0].addonVersions[?compatibilities[0].defaultVersion==`true`].addonVersion' \
     --output text
   ```

   (`prod-gardenio-eks` is on k8s 1.31 as of 2026-08-04 — re-check with
   `kubectl version`.)

2. **Set the tfvars** (in `prod-real.tfvars`, or a `-var` at apply):

   ```hcl
   coverage_efs_enabled           = true
   coverage_efs_csi_addon_version = "vX.Y.Z-eksbuild.N"   # from step 1
   # coverage_efs_storage_class_name = "gardenio-efs-coverage"  # default
   # coverage_efs_backup_enabled     = false                   # default
   ```

3. **Plan + review.** Expect exactly: 1 EFS filesystem, N mount targets (one
   per node subnet), 1 SG + its NFS ingress rules, 1 IAM role + policy
   attachment, 1 `aws_eks_addon.this["aws-efs-csi-driver"]`, 1
   `kubernetes_storage_class`. NOTHING else should change (the off→on switch is
   purely additive; the base addon set is byte-identical via
   `local.eks_base_addons`).

   ```bash
   cd gardenio/terraform/composition/canonical-prod
   terraform plan -var-file=prod-real.tfvars -out efs.plan
   ```

4. **Apply** (full apply — NOT `-target=module.gardenio`):

   ```bash
   terraform apply efs.plan
   ```

5. **Verify** the class + driver are live:

   ```bash
   kubectl get storageclass gardenio-efs-coverage
   kubectl -n kube-system get pods -l app=efs-csi-controller
   kubectl -n kube-system get pods -l app=efs-csi-node    # one per node
   ```

## Drive the first green coverage pipeline

Once applied, a `coverage=1` deploy binds the RWM PVC against
`gardenio-efs-coverage` automatically — `.rwx/deploy-rwx.yml` passes
`--param coverage-storage-class` (default `gardenio-efs-coverage`) which sets
`global.coverage.persistence.storageClass`. Dispatch:

```bash
rwx run gardenio-deploy-rwx --param coverage=1 --param instance-id=<x> ...
```

then the e2e + coverage-drain tasks (`.rwx/e2e.yml`) produce the first
non-zero merged coverage. This unblocks ka-0000003136 and the epic acceptance.

## Rollback

Set `coverage_efs_enabled = false` and apply. The StorageClass carries
`helm.sh/resource-policy: keep` and the filesystem/mount targets are
`module.efs` resources gated on the same flag — a disable removes them. If a
coverage PVC still references the class, delete that PVC first (it is derived,
reproducible data).

## Cost

Elastic-throughput EFS bills per-use with an IA lifecycle (`AFTER_7_DAYS` →
IA, `AFTER_1_ACCESS` → primary), so the standing cost between coverage runs is
near-zero. `coverage_efs_backup_enabled` (default false) adds AWS Backup if the
derived coverage data ever needs it (it normally does not).
