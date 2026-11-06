<!-- Audit deliverable for ka-0000003134 (parent epic ka-0000006754).
     Method: per-principal least-priv review + adversarial verification of each finding
     against the actual terraform. Advice refs: ka-0000001739 (IRSA web-identity),
     ka-0000007102 (AKIA tf exception). Date: 2026-06-10. -->

# canonical-prod secrets/IAM least-privilege audit

## 1. Executive summary

**Overall verdict: substantially least-privilege, with one critical credential-to-state leak that must be remediated before this composition is considered production-clean.**

The canonical-prod terraform composition demonstrates a strong baseline least-privilege posture. IAM roles are predominantly ARN-scoped, KMS access is delegated to single CMKs per data domain, and trust policies are tightly bound. The two architectural exceptions tracked in the parent epic are confirmed correct:

- **IRSA web-identity (ka-0000001739): CONFIRMED.** Every workload principal (agent, external-secrets, external-dns, cert-manager, aws-lb-controller, karpenter, ebs-csi) authenticates via `sts:AssumeRoleWithWebIdentity` with `StringEquals` conditions pinning both the OIDC `:sub` (specific `system:serviceaccount:<ns>:<sa>`) and `:aud` (`sts.amazonaws.com`). No static workload credentials exist. Node roles correctly use service-principal trust (`ec2.amazonaws.com`), and the EKS cluster role uses `eks.amazonaws.com`.
- **AKIA tf exception (ka-0000007102): CONTAINED.** The AWS provider and Kubernetes/Helm/kubectl providers use ambient credentials (STS default chain / `aws eks get-token` exec plugins). No static `AKIA` keys are committed or hardcoded; `*.tfvars`/`*.tfstate` are git-ignored; secrets thread in via `TF_VAR_*` at apply time. The historical "injected AKIA creds as K8s Secret" pattern is documented as resolved in favor of OIDC-issued scoped STS credentials.

**The one material exception to the otherwise-clean posture** is the edge-auth OIDC `client_secret`, which is decoded in Terraform and materialized in plaintext inside the `aws_verifiedaccess_trust_provider` resource state. This is an unencrypted credential leak to `terraform.tfstate` and is the single critical item.

### Counts by severity

| Severity | Count |
|---|---|
| Critical | 1 |
| High | 3 |
| Medium | 11 |
| Low | 4 |
| Info | 2 |
| Rejected (cleared) | 6 |

Per-principal verdicts: `agent-irsa`, `eks-cluster-node`, `rds`, `secrets-manager-module`, and the `akia-exception` provider config are **fully compliant**. `cert-manager-irsa` and `karpenter`/`kms-cmks` are **acceptable with minor follow-ups**. `external-secrets-irsa`, `external-dns-irsa`, and `aws-lb-controller-irsa` carry **real over-scoping** (mostly AWS-API-mandated wildcards or upstream-mirrored patterns). `edge-auth-secret` carries the **critical** finding.

---

## 2. Findings

### Critical

**OIDC client_secret plaintext leak to terraform state**
- Principal: `edge-auth-secret`
- File: `gardenio/terraform/modules/edge-auth/main.tf` (lines 128-146)
- What's wrong: `jsondecode(data.aws_secretsmanager_secret_version.oidc_client[0].secret_string)` (line 128) extracts the OIDC `client_secret` into a local (line 130) and passes it to `aws_verifiedaccess_trust_provider.oidc` (`client_secret = local.oidc_client_secret`, line 146). Despite the comment claiming plaintext "never lands in TF state for the secret itself," the resolved plaintext GitLab OIDC `client_secret` **does** land in the trust provider's resource state. The code at lines 107-111 explicitly acknowledges this AWS-provider limitation. State sensitivity on outputs does not protect state file contents.
- Recommended fix: Stop letting Terraform manage the OIDC credential. Either (A) add `lifecycle { ignore_changes = [oidc_options.client_secret] }` to the trust provider and pre-populate the secret out-of-band (the same operator-driven shell pattern already used for `aws_secretsmanager_secret_version` at lines 76-93), or (B) refactor to a `null_resource` + `local-exec` that calls the AWS API directly and reads back only the resource ID. Extend the existing placeholder + `ignore_changes` philosophy to the trust provider.

### High

**Route53 ChangeResourceRecordSets grants hostedzone/* in empty-zone fallback**
- Principal: `external-dns-irsa`
- File: `gardenio/terraform/modules/platform/external_dns.tf` (sid `ExternalDNSChangeRecords`, lines 64, 67-71)
- What's wrong: The resource ternary defaults to `arn:<partition>:route53:::hostedzone/*` when `var.public_zone_id` is empty, granting record mutation across all hosted zones in the account. A runtime `zoneIdFilters = []` (line 125) prevents practical exploitation today, but the IAM grant itself is unbounded (defense-in-depth masking an over-grant).
- Recommended fix: Make `var.public_zone_id` a required non-empty variable in prod (or fail Terraform validation when empty). A safe default for the resources list is empty (no resources), never `*`.

**Unguarded elasticloadbalancing listener and rule mutation on wildcard resources**
- Principal: `aws-lb-controller-irsa`
- File: `gardenio/terraform/modules/platform/aws_lb_controller.tf` (sid `ELBControllerListenerMutate`, lines 203-212)
- What's wrong: Grants `elasticloadbalancing:CreateListener/DeleteListener/CreateRule/DeleteRule` on `resources=['*']` with no conditions, unlike `ELBControllerLBMutate` (lines 254-273) which gates on `aws:ResourceTag/elbv2.k8s.aws/cluster`. This permits listener/rule mutation on any load balancer account-wide.
- Recommended fix: Add the same `aws:ResourceTag/elbv2.k8s.aws/cluster` (Null/`false`) condition used by `ELBControllerLBMutate` (lines 268-272) to `ELBControllerListenerMutate`.
- **Resolution (ka-0000003118, decision ka-0000004564 — ACCEPTED AS UPSTREAM-MANDATED):** The recommended fix is **infeasible** and was verified to break the controller. `CreateListener`/`CreateRule` create a *new* resource that carries no tags at IAM-evaluation time, so a `Null aws:ResourceTag/elbv2.k8s.aws/cluster` condition can never be satisfied → implicit-deny → listener/rule provisioning fails. The correct create-time key would be `aws:RequestTag`, which the [upstream v2.8 `iam_policy.json`](https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/v2.8.0/docs/install/iam_policy.json) deliberately does *not* apply to these four actions (it grants them `Resource:"*"` with no condition). Post-create listener/rule tagging is already gated by `ELBControllerListenerTagMutate`. No policy change; statement annotated inline with rationale + sources.

**AllowScopedResourceCreationTagging: CreateTags resource="*" unscoped by resource ARN**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (sid `AllowScopedResourceCreationTagging`, lines 253-273)
- What's wrong: `ec2:CreateTags` on `resources=['*']` guarded only by `aws:RequestTag` match + `ec2:CreateAction` whitelist. The `ec2:CreateAction` condition limits this to launch-time tagging, but the resource wildcard still permits tagging any EC2 resource during those operations. The adjacent `AllowScopedEC2InstanceActionsWithTags` (lines 220-251) already scopes ARNs to instance/fleet/launch-template/volume/network-interface/spot-instances-request.
- Recommended fix: Replace `resources=['*']` with the region/account-scoped ARN patterns already used in `AllowScopedEC2InstanceActionsWithTags` so `CreateTags` can only target Karpenter-provisioned resource types in the current region.

**AllowScopedResourceTagging: CreateTags on instance/* with no Karpenter-ownership discriminator**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (sid `AllowScopedResourceTagging`, lines 275-285)
- What's wrong: `ec2:CreateTags` on `instance/*` guarded only by `aws:ResourceTag/kubernetes.io/cluster/<cluster_name>=owned`. This is an *existing-tag* (ResourceTag) check, and EKS infra node-group instances also carry the cluster-ownership tag — so the controller could re-tag infra nodes, creating cross-contamination between Karpenter-provisioned and infra-managed instances.
- Recommended fix: Add a discriminator that only Karpenter nodes carry, e.g. require `aws:ResourceTag/karpenter.sh/nodepool` to exist, or consolidate this statement with `AllowScopedResourceCreationTagging` and drop the redundant one.

### Medium

**ListSecretVersionIds may be unnecessary (contradicts documented scope)**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (sid `ESOReadSecrets`, line 81)
- What's wrong: Grants `secretsmanager:ListSecretVersionIds` alongside `GetSecretValue`/`DescribeSecret`, but the file's own intent comment (lines 7-8) documents only `GetSecretValue + DescribeSecret`. Undocumented action exceeding stated requirements; exposes rotation-history metadata.
- Recommended fix: Verify via external-secrets upstream/audit logs whether ESO invokes `ListSecretVersionIds`. If not, remove it. If required, document the reason inline.
- **Resolution (ka-0000000383 — KEEP + DOCUMENT):** `ListSecretVersionIds` is part of the **upstream external-secrets AWS Secrets Manager provider's documented read policy** (verified against [external-secrets.io AWS SM provider docs](https://external-secrets.io/latest/provider/aws-secrets-manager/), whose sample policy lists `GetSecretValue`, `DescribeSecret`, `ListSecretVersionIds`, `ListSecrets`, `BatchGetSecretValue`, `GetResourcePolicy`). It is read-only and already **prefix-scoped** (not wildcard). Retained for upstream parity; the stale intent comment (lines 7-12) and the statement were updated inline to document the full read bundle. No policy change.

**No validation that additional_secret_arns match intended prefixes**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (line 90; var in `variables.tf` lines 99-110)
- What's wrong: `var.additional_secret_arns` (type `list(string)`, no validation) is `concat()`-ed directly into the policy resources. A caller can supply arbitrary/out-of-prefix/malformed ARNs and receive `GetSecretValue`+`DescribeSecret` on them, drifting from the documented `<prefix>/*` intent.
- Recommended fix: Add Terraform variable `validation` enforcing the canonical Secrets Manager ARN pattern, optionally warn on out-of-prefix ARNs, and document caller responsibility in the variable description.

**No validation that additional_kms_key_arns are appropriate**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (line 114; var lines 105-116)
- What's wrong: `kms:Decrypt`/`kms:DescribeKey` granted on `toset(compact(var.additional_kms_key_arns))` with no ARN-format, ownership, or relevance validation. A caller could pass overly broad or unrelated CMK ARNs. Actions are read-only, so this is a config-control gap, not an over-grant.
- Recommended fix: Add variable `validation` for the canonical CMK ARN pattern; consider a `kms:DescribeKey` data lookup to confirm keys are customer-managed in-account; document that only CMKs encrypting in-scope secrets should be listed.

**External-secrets ListSecrets wildcard (reconnaissance vector)**
- Principal: `edge-auth-secret` / `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (sid `ESOListSecrets`, lines 95-99)
- What's wrong: `secretsmanager:ListSecrets` on `resources=['*']` enables account-wide secret-name enumeration if the ESO pod is compromised. AWS does not support resource-level scoping for `ListSecrets`, so the wildcard is API-mandated; `GetSecretValue` is correctly prefix-scoped (lines 83-91), limiting actual exfiltration. (See also the LOW duplicate below — same statement, lower-impact framing.)
- Recommended fix: Accept the wildcard as an AWS limitation but add compensating controls: CloudTrail logging + alerting on out-of-scope `GetSecretValue` denials, restrict ESO egress to the Secrets Manager VPC endpoint, and/or add per-secret resource policies. The OIDC secret is double-locked via explicit ARN grant in `composition/canonical-prod/platform.tf:58`.

**Route53 List actions on wildcard resources**
- Principal: `external-dns-irsa`
- File: `gardenio/terraform/modules/platform/external_dns.tf` (sid `ExternalDNSListZones`, lines 74-83)
- What's wrong: `route53:ListHostedZones`, `ListResourceRecordSets`, `ListTagsForResource` on `resources=['*']`. `ListHostedZones` legitimately requires `*`, but `ListResourceRecordSets`/`ListTagsForResource` can be scoped to the managed zone ARN. Read-only; follows upstream external-dns guidance; mitigated at runtime by `zoneIdFilters`.
- Recommended fix: Scope `ListResourceRecordSets`/`ListTagsForResource` to the specific `hostedzone/<zone_id>` ARN, reusing the ternary from `ExternalDNSChangeRecords` (lines 67-71).

**Unguarded shield:CreateProtection / DeleteProtection on wildcard resources**
- Principal: `aws-lb-controller-irsa`
- File: `gardenio/terraform/modules/platform/aws_lb_controller.tf` (sid `ELBControllerReadOnly`, lines 108-109)
- What's wrong: Shield mutate verbs on `resources=['*']` with no guard. Mirrors upstream v2.8 verbatim; Shield API actions do not support ARN/resource-level IAM conditions, so the proposed tag condition is not directly applicable. Blast radius is limited by Shield's account-scoped API design.
- Recommended fix: Accept as upstream-mirrored if Shield integration is required; otherwise consider removing Shield verbs entirely if Shield Advanced is not in use for this cluster. Document the decision inline.

**Unguarded security group ingress/egress mutation on wildcard resources**
- Principal: `aws-lb-controller-irsa`
- File: `gardenio/terraform/modules/platform/aws_lb_controller.tf` (sid `ELBControllerSGMutate`, lines 127-130)
- What's wrong: `ec2:AuthorizeSecurityGroupIngress`/`RevokeSecurityGroupIngress` on `resources=['*']` with no conditions, while the parallel `ELBControllerSGRunTime` (lines 175-188) applies an `aws:ResourceTag/elbv2.k8s.aws/cluster` guard — an internal inconsistency. Mirrors upstream action-scoped model; role is OIDC-scoped to the controller SA.
- Recommended fix: Add the `aws:ResourceTag/elbv2.k8s.aws/cluster` condition to `ELBControllerSGMutate` to match `ELBControllerSGRunTime`, or merge the two statements.
- **Resolution (ka-0000003118, decision ka-0000004564 — ACCEPTED AS UPSTREAM-MANDATED):** Not an inconsistency — it is a deliberate two-statement split, verbatim from the [upstream v2.8 `iam_policy.json`](https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/v2.8.0/docs/install/iam_policy.json), which carries the same two ingress/revoke statements. The **unconditional** `ELBControllerSGMutate` manages the **worker-node** security groups, which the controller identifies by the `kubernetes.io/cluster/<name>` tag (NOT `elbv2.k8s.aws/cluster`). Adding the elbv2 guard would deny ingress mutation on the node SGs (they never carry that tag) and break LB→backend traffic. The **tag-gated** `ELBControllerSGRunTime` already covers the LB-owned SGs. No policy change; statement annotated inline with rationale + sources.

**WAFv2 / WAF-regional AssociateWebACL on wildcard resources without scope**
- Principal: `aws-lb-controller-irsa`
- File: `gardenio/terraform/modules/platform/aws_lb_controller.tf` (lines 98-105/111)
- What's wrong: A single statement mixes read (`Get*`) and mutate (`Associate*`/`Disassociate*`) WAF verbs on `resources=['*']` with no conditions. Unlike ELB statements, no AWS-managed `aws:ResourceTag` condition gates these mutations, so the "action-scoped at runtime" comment (lines 56-60) does not hold for WAF.
- Recommended fix: Split read vs mutate into separate statements; scope mutate operations to load balancer ARNs (`arn:aws:elasticloadbalancing:*:*:loadbalancer/net/*/*`, `.../app/*/*`) or add a tag condition if upstream expects wildcard.
- **Resolution (ka-0000003118, decision ka-0000004564 — ACCEPTED AS UPSTREAM-MANDATED):** The proposed LB-ARN scoping is **infeasible** for the WAF mutate verbs. `wafv2:AssociateWebACL`/`DisassociateWebACL` act on the **WebACL** resource (the customer-managed WebACL whose ARN is supplied dynamically via Ingress annotation), *not* on the load-balancer ARN — scoping to LB ARNs would silently deny every association. The WebACL ARNs are externally managed and unknown to this module, and per the [AWS WAF Service Authorization Reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_awswafv2.html) `wafv2:DisassociateWebACL` does not support resource-level scoping at all. This matches the [upstream v2.8 `iam_policy.json`](https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/v2.8.0/docs/install/iam_policy.json) (`Resource:"*"`, no condition). No policy change; statement annotated inline with rationale + sources. (Note: the read `Get*` verbs *could* be split into a separate statement cosmetically, but it yields no real blast-radius reduction since the mutate verbs must stay wildcard.)

**iam:GetServerCertificate / ListServerCertificates on wildcard resources**
- Principal: `aws-lb-controller-irsa`
- File: `gardenio/terraform/modules/platform/aws_lb_controller.tf` (sid `ELBControllerReadOnly`, lines 68-69)
- What's wrong: Read-only IAM server-certificate enumeration on `resources=['*']`, allowing discovery of all certificates in the account.
- Recommended fix: Scope to known certificate ARNs if pinned, or accept the read-only wildcard with downstream K8s-level controls limiting which certs the controller uses. Document if accepting.

**AllowPricingReadActions: GetProducts resource="*" with no regional filtering**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (sid `AllowPricingReadActions`, lines 335-339)
- What's wrong: `pricing:GetProducts` on `resources=['*']` with zero conditions, while `AllowRegionalReadActions` (lines 305-325) demonstrates the regional-conditioning pattern. Read-only and Pricing API is global by design.
- Recommended fix: Add `condition { test="StringEquals" variable="aws:RequestedRegion" values=[data.aws_region.current.name] }` to align with the module's stated "scope to this cluster + region" intent.

**AllowInstanceProfileReadActions: GetInstanceProfile resource="*" lacks cluster tag guard**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (sid `AllowInstanceProfileReadActions`, lines 410-414)
- What's wrong: `iam:GetInstanceProfile` on `resources=['*']` with no conditions, in contrast to the scoped create/modify instance-profile statements (lines 368-407). Read-only but leaks profile metadata outside cluster scope.
- Recommended fix: Add the `kubernetes.io/cluster/<cluster_name>=owned` + region `aws:ResourceTag` conditions used elsewhere, or narrow `resources` to `aws_iam_instance_profile.node.arn`.

**Node role attaches AmazonSSMManagedInstanceCore (necessity unverified)**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (lines 107-110)
- What's wrong: `AmazonSSMManagedInstanceCore` is attached unconditionally to the Karpenter node role. The inline comment claims this matches the EKS infra node group, but grep confirms the EKS module attaches no SSM policy — the comment is factually wrong. Nodes are ephemeral/scale-to-zero, so SSM Session/Patch Manager workflows are likely unused; no opt-out flag exists.
- Recommended fix: Confirm whether SSM (Session Manager / Run Command / Patch Manager) is used for these nodes. If not, remove the attachment. If yes, correct the comment and document the use case; consider restricting to read-only + EC2 Messages.
- **Resolution (ka-0000000383 — KEEP + DOCUMENT):** Confirmed the EKS module (`modules/eks/main.tf`) attaches **no** SSM policy to its infra node group (only `AmazonEKSWorkerNodePolicy`/`AmazonEKS_CNI_Policy`/`AmazonEC2ContainerRegistryReadOnly`), so the attachment is an **addition**, not a match — and no factually-wrong inline comment survived in `modules/karpenter/main.tf` (the README's "…plus `AmazonSSMManagedInstanceCore`" phrasing is accurate). `AmazonSSMManagedInstanceCore` is the **upstream-standard Karpenter node-role attachment** ([karpenter.sh getting-started CloudFormation](https://karpenter.sh/docs/reference/cloudformation/)) and provides SSM **Session Manager** break-glass access — the only practical shell into these SSH-less/bastion-less ephemeral Spot nodes for prod incident response. Retained for that real capability; an accurate inline comment was added. No policy change.

**AllowRegionalReadActions: Describe* on resource="*" (acceptable, tag-filter optional)**
- Principal: `karpenter`
- File: `gardenio/terraform/modules/karpenter/main.tf` (sid `AllowRegionalReadActions`, lines 306-325)
- What's wrong: `ec2:Describe*` (9 actions) on `resources=['*']` guarded only by `aws:RequestedRegion`. Matches upstream cloud-provider-aws and is appropriate for read-only discovery; not maximally least-privilege relative to the cluster discovery tags.
- Recommended fix: Acceptable as-is for MVP. For hardened prod, evaluate adding `aws:ResourceTag/karpenter.sh/discovery` conditions where the specific `Describe*` actions support resource-tag condition keys.

**Missing sensitive=true on edge-auth trust provider outputs**
- Principal: `edge-auth-secret`
- File: `gardenio/terraform/modules/edge-auth/outputs.tf` (lines 11-14; also 36-39, 41-44)
- What's wrong: `trust_provider_id` (and the OIDC secret ARN/name outputs) lack `sensitive = true`, despite the `main.tf:110` comment claiming "hence the `sensitive = true` on outputs." The IDs themselves aren't secret, but they relate to a resource whose state contains the plaintext `client_secret`; the marker is the documented defense-in-depth that was never implemented.
- Recommended fix: Add `sensitive = true` to the trust-provider and OIDC-secret outputs and any downstream composition outputs that surface them.

### Low

**ListSecrets action uses wildcard resource (metadata-only)**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (sid `ESOListSecrets`, lines 94-99)
- What's wrong: `secretsmanager:ListSecrets` on `resources=['*']` exposes metadata (names, descriptions, rotation status) for all account secrets. AWS does not support resource-level filtering for this action; escalation is blocked by scoped `GetSecretValue`. (Same statement as the MEDIUM recon-vector finding; lower framing because only metadata is disclosed.)
- Recommended fix: Treat as a known AWS limitation; document and periodically audit who holds the role. Compensating controls per the MEDIUM finding above.

**KMS DescribeKey may be unnecessary**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (lines 110-113)
- What's wrong: `kms:DescribeKey` granted alongside `kms:Decrypt` on whitelisted CMK ARNs. Decrypt alone suffices for retrieving secrets; DescribeKey exposes CMK metadata. Scope is constrained to listed ARNs.
- Recommended fix: Verify whether ESO invokes DescribeKey (key health checks/diagnostics). If not, grant only `kms:Decrypt`. If yes, document inline.
- **Resolution (ka-0000000383 — REMOVED):** ESO's AWS Secrets Manager provider **never calls KMS directly** — decryption is performed server-side by Secrets Manager during `GetSecretValue` (which invokes `kms:Decrypt` in the caller's context), and `DescribeKey` is not on any ESO code path (confirmed against the [upstream provider docs](https://external-secrets.io/latest/provider/aws-secrets-manager/), which do not require it). `kms:DescribeKey` was **dropped** from `ESODecryptKMS`, leaving `kms:Decrypt` only. Safe: `DescribeKey` removal cannot affect `GetSecretValue`. (The crew `agent_irsa.tf` grant retains `DescribeKey` deliberately for ad-hoc aws-CLI diagnostics; its cross-reference comment was corrected.)

**GetChange accepts change/* wildcard (verify necessity)**
- Principal: `cert-manager-irsa`
- File: `gardenio/terraform/modules/platform/cert_manager.tf` (sid `CertManagerGetChange`, lines 65-70, action at line 69)
- What's wrong: `route53:GetChange` on `arn:*:route53:::change/*`. AWS does not support resource-level filtering for `GetChange`, so the wildcard is unavoidable; the policy mirrors official cert-manager DNS-01 docs. The open question is whether cert-manager needs `GetChange` at all.
- Recommended fix: Confirm against cert-manager DNS-01 Route53 upstream docs. If used for change polling (expected), accept the wildcard as API-mandated and document. If unused, remove the statement.
- **Resolution (ka-0000000383 — KEEP + DOCUMENT):** Confirmed against the [upstream cert-manager Route53 DNS-01 docs](https://cert-manager.io/docs/configuration/acme/dns01/route53/): `route53:GetChange` on `change/*` **is** part of the canonical recommended IAM policy, and cert-manager's solver polls `GetChange` after each `ChangeResourceRecordSets` to wait for the record to reach **INSYNC** before asking ACME to validate. The `change/*` wildcard is API-mandated (opaque change IDs, no resource-level scoping). Retained as required; documented inline. No policy change.

**Overly broad KMS root statement on secrets_manager CMK**
- Principal: `edge-auth-secret` / `kms-cmks`
- File: `gardenio/terraform/modules/kms-cmks/main.tf` (lines 38-49)
- What's wrong: `kms:*` on `resources=['*']` granted to the account root principal. This is the canonical AWS anti-lockout pattern, but it does grant unrestricted key operations to root.
- Recommended fix: Accepted-risk pattern. Optionally split admin (`Create*`/`Delete*`/`ScheduleKeyDeletion`) from data-plane verbs, or add an account/condition scope as defense-in-depth. Not required.

### Info

**Policy description references only the primary prefix**
- Principal: `external-secrets-irsa`
- File: `gardenio/terraform/modules/platform/external_secrets.tf` (line 128)
- What's wrong: `description = "...scope=${var.secrets_prefix}"` omits `var.additional_secrets_prefixes`. The description is immutable post-creation (lines 124-127), so it can't be safely changed without replace; the actual policy document (lines 83-91) correctly covers all prefixes via `local.external_secrets_all_prefixes_normalized`. Documentation clarity only — no privilege impact.
- Recommended fix: Note the full scope in the module README/CLAUDE.md or via a tag/output, or annotate `scope=<primary> [+ additional]` if a future replace is acceptable.

**KMS rotation + prevent_destroy + IRSA delegation model (posture confirmation)**
- Principal: `kms-cmks`
- File: `gardenio/terraform/modules/kms-cmks/main.tf`
- What's right: All three CMKs (rds, s3, secrets_manager) have `enable_key_rotation = true` and `lifecycle.prevent_destroy = true`; usage permissions flow through IAM identity policies on IRSA roles rather than key-policy USER grants; key administrators are variable-scoped and only rendered when supplied. Informational — correct by design.

---

## 3. Confirmed-correct posture (least-privilege done right)

**agent-irsa**
- OIDC assume-role conditions pin both `:sub` (`system:serviceaccount:gardenio-system:gardenio-agent`) and `:aud` (`sts.amazonaws.com`) — `agent_irsa.tf` lines 33-43.
- S3 object permissions minimal (`Put/Get/Delete/AbortMultipartUpload`) and scoped to `<bucket>/*`, no cross-bucket wildcard — lines 57-67.
- `s3:ListBucket` correctly on the bucket ARN (not `/*`) — lines 70-75.
- KMS grant scoped to the single S3 CMK with only `GenerateDataKey/Decrypt/DescribeKey`; no `kms:*` — lines 80-89.
- S3 bucket: all four public-access-block flags set, default SSE-KMS with the customer CMK + bucket key, versioning + `prevent_destroy`, lifecycle expiry/multipart-abort rules.
- No wildcard resource grants anywhere; no committed static credentials.

**external-secrets-irsa**
- IRSA trust policy constrained to `system:serviceaccount:external-secrets:external-secrets` + `aud=sts.amazonaws.com` — lines 37-61.
- Prefix normalization via `trimprefix()` matches canonical Secrets Manager ARN shape — lines 27-34.
- Dynamic KMS statement emits only when `additional_kms_key_arns` is non-empty — lines 105-116.

**external-dns-irsa**
- Assume-role policy enforces OIDC `:sub` (external-dns SA) and `:aud` conditions — lines 25-49.

**cert-manager-irsa**
- `ChangeResourceRecordSets`/`ListResourceRecordSets` scoped to the configured zone ARN; empty-zone fallback is dead code because the ClusterIssuer only deploys when `public_zone_id != ""` — lines 72-84.
- Assume-role policy scoped to `system:serviceaccount:cert-manager:cert-manager` + `aud` — lines 27-51.
- Zone-discovery `ListHostedZones*` wildcard is AWS-API-mandated and read-only — lines 86-91.
- No static credentials; ACME keys in K8s Secrets, not state.

**aws-lb-controller-irsa**
- Tag-condition guards correctly applied to the majority of mutate statements (`SGTagOnCreate`, `SGTagMutate`, `CreateLB`, `TagMutate`, `LBMutate`, `TGTags`).
- Action verbs faithfully mirror upstream v2.8 `iam_policy.json`; no extraneous verbs.
- IRSA assume-role scoped to `kube-system:aws-load-balancer-controller` + `aud`; no static creds.

**karpenter**
- `iam:PassRole` scoped to the node role ARN with `iam:PassedToService=ec2.amazonaws.com` — lines 354-366.
- `RunInstances`/`CreateFleet` provisioning scoped by region-aware ARN patterns + cluster/nodepool tag conditions — lines 183-251.
- SQS interruption-queue access scoped to the specific queue ARN — lines 342-351.
- Controller IRSA trust bound to `karpenter:karpenter` SA + `aud`; node role trust limited to `ec2.amazonaws.com`; EKS access entry `type=EC2_LINUX`.

**eks-cluster-node**
- Node role carries exactly the three canonical AWS-managed policies; cluster role exactly `AmazonEKSClusterPolicy`; EBS CSI exactly `AmazonEBSCSIDriverPolicy`.
- EBS CSI IRSA trust scoped to `kube-system:ebs-csi-controller-sa` + `aud`; node/cluster trust scoped to EC2/EKS service principals.
- OIDC provider `client_id_list = ['sts.amazonaws.com']` only; EKS secrets encryption via caller CMK scoped to `['secrets']`.
- No inline policies or wildcard actions in any role; `/gardenio/` path isolation; CA cert output marked sensitive; no embedded credentials.

**rds**
- `manage_master_user_password=true` with `master_user_secret_kms_key_id` CMK — no plaintext password in state — lines 126-130.
- Enhanced-monitoring role uses AWS-managed `AmazonRDSEnhancedMonitoringRole`, conditionally created, trust scoped to `monitoring.rds.amazonaws.com`.
- Storage + Performance Insights encrypted with the customer CMK; `publicly_accessible=false`; SG-restricted ingress; backups/deletion-protection/final-snapshot configured; CloudWatch log export enabled.
- No inline policies or wildcard resources.

**kms-cmks** — see Info above (rotation, prevent_destroy, IRSA delegation, conditioned admins).

**secrets-manager-module**
- Paths-only architecture: no `secret_value` input, value populated out-of-band — no plaintext in state.
- CMK encryption mandatory (required `kms_key_arn`, no AWS-managed fallback); recovery-window validation (0 or 7-30); path-prefixed naming; no IAM policies declared (caller responsibility); no static credentials.

**edge-auth-secret (positives)**
- OIDC secret explicitly granted by ARN in the ESO IRSA `additional_secret_arns` (belt-and-suspenders over prefix scope) — composition lines 49-59.
- Secret shell uses `prevent_destroy = true`, 30-day recovery, and placeholder + `ignore_changes` pattern.
- OIDC secret encrypted with the secrets_manager CMK (rotation + prevent_destroy).

**akia-exception (provider config)**
- AWS provider uses ambient credentials (region + default_tags only, no static keys).
- Kubernetes/Helm/kubectl providers use `aws eks get-token` exec IRSA auth.
- `terraform.tfvars.example` documents `TF_VAR_*` threading with a hard "do not commit real tfvars" rule.
- No committed static credentials; `*.tfvars`/`*.tfstate` git-ignored; "static credentials" references in code are explanatory comments.
- Secrets Manager ARNs explicitly granted (no prefix-wildcard reliance); dedicated CMK for secrets encryption; prod-safe variable defaults (rejects `0.0.0.0/0`, empty key-admin default); IRSA resolution documented per ka-0000003915.

---

## 4. Considered & cleared (refuted on verification)

- **external-secrets-irsa — "Redundant ARN glob patterns":** Cleared. `secret:${p}/*` (hierarchical) and `secret:${p}*` (flat) are both intentional and non-redundant; prefix is normalized; no over-grant.
- **aws-lb-controller-irsa — "Unguarded security group creation":** Cleared. `CreateSecurityGroup` tagging is atomic; `SGTagOnCreate` enforces `aws:RequestTag`, runtime mutations gate on existing `aws:ResourceTag`; standard upstream pattern, no observable untagged window.
- **karpenter — "Missing explicit Deny on TerminateInstances outside cluster":** Cleared. `TerminateInstances` (lines 288-303) is condition-scoped to `kubernetes.io/cluster/<cluster_name>=owned`; condition-false implies implicit deny — explicit Deny is optional hardening, not a least-priv fix.
- **kms-cmks — "Overly broad resource scope in AllowKeyAdministration":** Cleared. `for_each = local.keys` produces a per-key policy; `resources=['*']` in a KMS *key policy* applies only to the attached key, isolated per-CMK.
- **edge-auth-secret — "KMS CMK encryption missing for OIDC secret":** Cleared. Both compositions unconditionally wire `oidc_secret_kms_key_id = module.kms_cmks.secrets_manager_key_arn`, and the CMK + secret shell both set `prevent_destroy = true`; finding examined only the module layer.

---

## 5. Recommendations & follow-up beads

**Must-fix (critical/high — credential leak and unbounded mutation grants):**
1. **[FIX, critical]** Remove the edge-auth OIDC `client_secret` from Terraform-managed state — apply `ignore_changes = [oidc_options.client_secret]` + out-of-band population, or move credential injection to a `null_resource`/`local-exec` that never reads the secret back. File: `modules/edge-auth/main.tf` lines 128-146. *Bead: edge-auth client_secret state-leak remediation.*
2. **[FIX, high]** Require non-empty `public_zone_id` in prod (or fail validation) and default the external-dns ChangeRecords resources to an empty list, never `hostedzone/*`. File: `modules/platform/external_dns.tf` lines 67-71. *Bead: external-dns zone-scope hardening.*
3. **[~~FIX~~ → ACCEPTED, high]** ~~Add the `aws:ResourceTag/elbv2.k8s.aws/cluster` guard to `ELBControllerListenerMutate`.~~ **Resolved ka-0000003118 / decision ka-0000004564: tag-guard verified infeasible (create-time `aws:ResourceTag` is unsatisfiable; upstream v2.8 grants `Resource:"*"` unconditionally). Accepted as upstream-mandated; statement annotated inline.** File: `modules/platform/aws_lb_controller.tf` (sid `ELBControllerListenerMutate`).
4. **[FIX, high]** Scope karpenter `AllowScopedResourceCreationTagging` `CreateTags` to the region/account ARN patterns from `AllowScopedEC2InstanceActionsWithTags`. File: `modules/karpenter/main.tf` lines 253-273. *Bead: karpenter CreateTags ARN scoping.*
5. **[FIX, high]** Add a Karpenter-ownership discriminator (`karpenter.sh/nodepool` exists) to `AllowScopedResourceTagging`, or consolidate with the creation-tagging statement, to prevent re-tagging EKS infra nodes. File: `modules/karpenter/main.tf` lines 275-285. *Bead: karpenter post-create tagging discriminator.*

**Should-fix (medium — consistency, validation, defense-in-depth):**
6. **[FIX]** Add `sensitive = true` to edge-auth trust-provider and OIDC secret outputs (`modules/edge-auth/outputs.tf` lines 11-14, 36-39, 41-44). *Pairs with #1.*
7. **[~~FIX~~ → ACCEPTED]** ~~Add `aws:ResourceTag` guard to `ELBControllerSGMutate` and split WAF read/mutate with scoped ARNs.~~ **Resolved ka-0000003118 / decision ka-0000004564: both verified infeasible — `ELBControllerSGMutate` intentionally manages worker-node SGs (`kubernetes.io/cluster/*`, not `elbv2.k8s.aws/cluster`); WAF `Associate/DisassociateWebACL` act on the customer-managed WebACL, not the ALB, so LB-ARN scoping silently denies. Accepted as upstream-mandated; statements annotated inline.** File: `modules/platform/aws_lb_controller.tf`.
8. **[FIX]** Add `aws:RequestedRegion` to karpenter `AllowPricingReadActions` (lines 335-339) and cluster/region tag guards (or node-profile ARN) to `AllowInstanceProfileReadActions` (lines 410-414). *Bead: karpenter read-action regional/tag scoping.*
9. **[FIX]** Add Terraform `validation` blocks for `additional_secret_arns` and `additional_kms_key_arns` (ARN-format, optional prefix/ownership checks). File: `modules/platform/external_secrets.tf` + `variables.tf`. *Bead: ESO input ARN validation.*
10. **[VERIFY→FIX → DONE, ka-0000000383]** Confirmed necessity of each against upstream sources:
    - `secretsmanager:ListSecretVersionIds` (`external_secrets.tf`) — **KEEP** (upstream ESO read-bundle, read-only, prefix-scoped); intent comment + statement documented inline.
    - karpenter node `AmazonSSMManagedInstanceCore` (`karpenter/main.tf`) — **KEEP** (upstream-standard node role; Session Manager break-glass into SSH-less ephemeral nodes); accurate inline comment added, EKS-parity claim corrected.
    - `kms:DescribeKey` (`external_secrets.tf` `ESODecryptKMS`) — **REMOVED** (ESO never calls KMS directly; `Decrypt` alone suffices for SM server-side decrypt).
    - cert-manager `route53:GetChange` (`cert_manager.tf`) — **KEEP** (upstream DNS-01 INSYNC polling; `change/*` API-mandated); documented inline.
    See each finding's per-item Resolution above for evidence.

**Accepted-risk / document only (no code change required unless policy dictates):**
- ESO `secretsmanager:ListSecrets` wildcard — AWS-API-mandated; add CloudTrail alerting + VPC-endpoint egress restriction as compensating controls, and document.
- LB-controller `shield:Create/DeleteProtection`, `iam:CreateServiceLinkedRole` (rejected as a violation), `iam:Get/ListServerCertificates` wildcards — upstream-mirrored / API-scoped; document the upstream-parity decision. Optionally drop Shield verbs if Shield Advanced is unused.
- karpenter `AllowRegionalReadActions` `Describe*` wildcard — upstream pattern; optional tag-filter for hardened prod.
- KMS account-root `kms:*` statement — canonical anti-lockout pattern; accept (optional admin/data-plane split as defense-in-depth).
- ESO immutable policy description omitting additional prefixes — document full scope in README/CLAUDE.md (info).

**Cross-cutting follow-up:** Enable CloudTrail data events on Secrets Manager and KMS for the canonical-prod account and add alerting on out-of-scope `GetSecretValue`/`Decrypt` denials, to compensate for the AWS-API-mandated `ListSecrets`/`GetChange` wildcards that cannot be tightened in IAM. *(Status ka-0000000383: this is an operator/`terraform apply` action against the canonical-prod account — data-event selectors on SM + KMS plus CloudWatch metric-filter alarms — not an IAM edit, so it is tracked as a dedicated follow-up bead under the security-audit epic ka-0000003872 rather than bundled into the action-necessity IAM MR.)* **Implemented (ka-0000007906):** `composition/canonical-prod/secrets_kms_cloudtrail_data_events.tf` — a single-region trail with `AWS::SecretsManager::Secret` + `AWS::KMS::Key` data-event selectors, an always-on AccessDenied metric-filter alarm over `GetSecretValue`/`Decrypt`/`GenerateDataKey`, and an operator-seeded out-of-scope-principal alarm, both routed to the existing ops SNS/Slack channel (the FinOps Slack-bridged topic by default). Ships **inert** (`var.secrets_kms_cloudtrail_data_events_enabled=false`); go-live is the operator root-targeted apply (account-wide data-event cost), outside `module.gardenio`.