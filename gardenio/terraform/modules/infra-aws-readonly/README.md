# infra-aws-readonly

Read-only AWS for the infra tier (gd-83VraF4SeC, plan gd-xiV5hyhPrG, decision D3
`irsa-readonly`: full read of the account, secret paths denied). The `infra` agent can
diagnose AWS (`ec2`, `autoscaling`, `ecr`, `route53`, `cloudwatch`, ...) and has no path to
secret material.

## The choice, and why

| Option | Verdict |
|---|---|
| IRSA (projected SA token + IAM OIDC provider) | Impossible today. The k3s API server issuer is `https://kubernetes.default.svc.cluster.local`, which STS cannot fetch, and no IAM OIDC provider exists. Fixing it is a k3s server-flag change plus a rolling replace of every server. |
| IAM Roles Anywhere | Needs a private CA, trust anchor, profile, and a client certificate delivered to the pod as **files** plus `aws_signing_helper` in the image. The controller injects role secrets as environment variables only (`applyRoleSecrets`), so this needs a runtime-authz controller change. |
| Node role over IMDS | **Rejected.** Every pod can assume it today; PR #270 closes that. |
| **Scoped static key in a Secret** | **Chosen.** It rides the mechanism that already exists: a role bead `secrets` entry injects `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` from a K8s Secret. The `aws` CLI is already in the agent image (pinned `AWSCLI_VERSION`, `images/agent/install/versions.env`), so no image change. |

Accepted trade-off: the key is long-lived, and anyone running code in an infra pod can read
it. So the **policy is the boundary**, and the key is rotated (below) and revocable per
subject.

## What it creates

| Resource | Notes |
|---|---|
| `aws_iam_group.this` | `gardenio-infra-aws-readonly` |
| `aws_iam_group_policy_attachment` x2 | AWS-managed `ReadOnlyAccess`, plus the Deny policy below |
| `aws_iam_policy.deny_secret_reads` | Explicit Deny: `secretsmanager:GetSecretValue`/`BatchGetSecretValue`, `ssm:GetParameter*` (the whole read family; IAM cannot condition on `WithDecryption`), `kms:Decrypt`/`ReEncryptFrom`, `lambda:GetFunction`/`GetFunctionConfiguration`/`ListFunctions`/`ListVersionsByFunction` (code URL; env vars in clear), `iam:GetCredentialReport`, `ecr:GetAuthorizationToken`/`BatchGetImage`/`GetDownloadUrlForLayer`, `ec2:GetPasswordData`, `ssm:ListCommands`/`ListCommandInvocations`/`GetCommandInvocation` (Run Command output history), `sts:GetSessionToken`, `logs:StartQuery`/`GetQueryResults`/`StartLiveTail`; `s3:GetObject`/`GetObjectVersion` on `arn:aws:s3:::*/*` (every bucket; listing stays allowed); `logs:GetLogEvents`/`FilterLogEvents` on every log group except `readable_log_groups` (`NotResource`) |
| `aws_iam_policy.cost_and_metrics_read` + attachment | Allow (gd-brIH1y5J9S): Cost Explorer `ce:Get*`/`Describe*`/`List*`; `cloudwatch:GetMetricData`/`GetMetricStatistics`/`ListMetrics`/`DescribeAlarms`; `logs:GetLogEvents`/`FilterLogEvents` on `readable_log_groups` (default: the three `/aws/lambda/gardenio-*` groups: edge-probe, uptime-probe, support-forward) |
| `aws_iam_user.subject[...]` + group membership | one per entry of `subjects` (`gardenio/gardenio-infra-reader` now; P6 appends `gardenio/gardenio-infra-admin`), `gardenio-<serviceaccount>-aws` |

No `aws_iam_access_key`: the key is minted out of band so its secret half is never in
terraform state (the state bucket is itself a denied path).

### Known residuals (the denylist cannot be proven complete)

`ReadOnlyAccess` is wide. Still readable: DynamoDB items, EC2 user-data and launch template
versions (today's user-data holds no secret: it fetches the k3s token from SSM at boot), and
whatever other service-specific read returns data. S3 object bytes and CloudWatch Logs
content are denied everywhere except the groups in `readable_log_groups`; another log group
needed for diagnosis is added there by exact name (no wildcards, enforced), and a bucket by
explicit ARN in a follow-up. Re-check this list whenever a new service that holds secrets
lands.

## OPERATOR steps (verbatim order)

```
# 1. apply from a tree equal to origin/main (profile gardenio, us-east-1)
./scripts/tf-gardenio.sh plan  -target=module.infra_aws_readonly   # expect 6 creates, 0 changes, 0 destroys
./scripts/tf-gardenio.sh apply -target=module.infra_aws_readonly

# 2. mint the key and write the Secret without the value touching argv, a file or the terminal
aws iam create-access-key --profile gardenio \
  --user-name gardenio-gardenio-infra-reader-aws \
  --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text \
| jq -Rn '(input | split("\t")) as $k | {apiVersion:"v1", kind:"Secret",
    metadata:{name:"infra-aws-readonly", namespace:"gardenio"},
    stringData:{"access-key-id":$k[0], "secret-access-key":$k[1]}}' \
| kubectl create -f -
```

Use `kubectl create`, never `apply`: `apply` copies the whole object, `stringData`
included, into the `last-applied-configuration` annotation, which anything with `get` on
that Secret can read back.

3. Point the `infra` role bead (gd-VINFcACohF; admin-gated, written by guarded SQL with an
   AUDIT RECORD comment) at it. The secret name must start with the role name plus `-`:
   - `secrets`: `[{"env":"AWS_ACCESS_KEY_ID","secret":"infra-aws-readonly","key":"access-key-id"},{"env":"AWS_SECRET_ACCESS_KEY","secret":"infra-aws-readonly","key":"secret-access-key"}]`
   - `env`: `[{"name":"AWS_DEFAULT_REGION","value":"us-east-1"}]`
4. Live check: spawn an `infra` agent. It runs `aws sts get-caller-identity` and
   `aws ec2 describe-instances` (both must succeed), then
   `aws secretsmanager get-secret-value --secret-id gardenio/kd-beads-token` (must fail
   `AccessDeniedException`). Report the error class only, never output.

Admin tier (P6): add `gardenio/gardenio-infra-admin` to `subjects`, mint its own key, and
name its Secret `infra-admin-aws-readonly` (role `infra-admin` prefix rule).

## Rotation (every 90 days, or on any suspicion)

`aws iam list-access-keys --user-name ...` shows key age. Create a second key (an IAM user
holds at most two), replace the Secret with the new key by running step 2 with `kubectl replace -f -` in place
of `kubectl create -f -` (never `apply`), restart the infra pods, then
`aws iam update-access-key --status Inactive` on the old key, and delete it once each
restarted infra pod has made one AWS call on the new key; reactivate the old one if a call fails.

Emergency revoke: **remove the user from the group** (`aws iam remove-user-from-group
--group-name gardenio-infra-aws-readonly --user-name <user>`), which drops ReadOnlyAccess
and the Deny together and takes effect for the whole identity, then deactivate and delete the
key. Deactivating the key alone is not enough if a session token was minted from it;
`sts:GetSessionToken` is denied, but a revoke should not depend on that.

## Rollback

`./scripts/tf-gardenio.sh apply -destroy -target=module.infra_aws_readonly` (first delete
any access keys: `aws iam list-access-keys` / `delete-access-key` per user, since terraform
does not own them), then remove the `secrets`/`env` entries from the role bead and
`kubectl -n gardenio delete secret infra-aws-readonly`.

## Tests

`terraform test` (mock provider, plan only; no credentials): the Deny is attached beside
ReadOnlyAccess, every statement is a Deny, the S3 object deny covers `arn:aws:s3:::*/*`, each secret-returning action has its own assertion (removing any one
fails the test; checked by mutation), positive controls (`ec2:DescribeInstances`,
`sts:GetCallerIdentity`, `s3:ListBucket`, `logs:DescribeLogGroups`, ...) are not denied, `subjects` is a list, and bad
input is rejected. CI: `checks.yml` terraform job.
