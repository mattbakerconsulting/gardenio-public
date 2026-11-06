# modules/ecr-pull-through-cache

Mirror upstream public registries — **GitHub Container Registry (GHCR)** and
**Docker Hub** — into this account's ECR via [pull-through cache rules][ptc], so
EKS node image pulls land on the **free S3-gateway / ECR-interface endpoint
path** instead of traversing the NAT instance.

This is the highest-leverage single NAT-byte reduction for image pulls
(ADR [`docs/research/canonical-prod-nat-elimination.md`][adr] §6, edges
**P3 / P6 / P14**; task `ka-0000005943`).

## How a pull-through cache works

A pull-through cache rule maps an ECR repository prefix to an upstream registry.
The **first** pull of `<account>.dkr.ecr.<region>.amazonaws.com/<prefix>/<upstream-path>:<tag>`
fetches the image from the upstream and stores it under that prefix in this
account's ECR. Every **subsequent** pull (by any node) is served straight from
ECR over the VPC endpoint — **$0 NAT bytes**, and no Docker Hub rate-limit
exposure.

To actually move the bytes off NAT, node containerd must resolve pulls of
`ghcr.io/...` / `docker.io/...` to the ECR cache URL. That node-wiring is done by
`modules/karpenter` (`var.node_registry_mirror`, task ka-0000002900): when the
composition sets `node_registry_mirror.enabled` — gated on the SAME
`ecr_pull_through_cache_enabled` flag so the two land in lockstep — the agent +
ephemeral EC2NodeClass userData drops a transparent containerd registry-mirror
(`/etc/containerd/certs.d/<upstream>/hosts.toml`, `override_path=true`) plus an
`ecr-mirror-login` systemd timer that refreshes the ECR bearer token into those
hosts.toml (the kubelet ECR credential provider authenticates only the original
`docker.io`/`ghcr.io` ref, never the ECR mirror host — containerd/containerd#11385).
The hosts.toml sets no top-level `server=`, so any mirror error falls back to the
real upstream automatically — the mirror is inert (bytes still go over NAT, as
today) until this module's cache rules + credentials + first-pull IAM exist.

## What this module provisions

| Resource | Purpose |
|---|---|
| `aws_secretsmanager_secret.upstream` (per credentialed upstream) | Paths-only credential secret, named `ecr-pullthroughcache/<env>-<key>` (the AWS-mandated prefix). **Value populated out-of-band.** |
| `aws_ecr_pull_through_cache_rule.this` (per upstream) | The cache rule mapping `<prefix>` → upstream registry. |
| `aws_iam_policy.node_pull_through` | Grants the EKS node role `CreateRepository` + `BatchImportUpstreamImage` (+ read verbs) scoped to the cache prefixes — the managed `AmazonEC2ContainerRegistryReadOnly` policy grants neither, and the **first** pull needs both. Output as `node_policy_arn`; the composition attaches it to the Karpenter node role. |

## Credentials are paths-only — populate out of band

AWS **requires** a credential for both the GHCR and Docker Hub upstreams. This
module creates the secret containers only (same convention as
`modules/secrets-manager`); it never receives plaintext. After apply, write the
value as JSON:

```json
{ "username": "<user>", "accessToken": "<token-or-PAT>" }
```

- **GHCR**: a GitHub username + a PAT (classic) with `read:packages`.
- **Docker Hub**: a Docker Hub username + an access token (read-only).

```sh
aws secretsmanager put-secret-value \
  --secret-id ecr-pullthroughcache/<env>-ghcr \
  --secret-string '{"username":"...","accessToken":"..."}'
```

`enabled=false` (the default) provisions nothing. Apply order: create the
secrets (this module, `enabled=true`), populate their values, then the cache
rules validate against live upstream creds. `terraform validate` runs need no
values.

## Inputs

| Name | Type | Default | Description |
|---|---|---|---|
| `env_name` | string | — | Short environment identifier. |
| `enabled` | bool | `false` | Master toggle. |
| `kms_key_arn` | string | `null` | CMK for the credential secrets. |
| `recovery_window_days` | number | `30` | Secret soft-delete window (0 or 7-30). |
| `upstreams` | map(object) | GHCR + Docker Hub | Upstreams to mirror, keyed by logical name. |
| `common_tags` | map(string) | `{}` | Tags merged onto every resource. |

## Outputs

| Name | Description |
|---|---|
| `enabled` | Whether the cache is provisioned. |
| `node_policy_arn` | First-pull cache-populate policy ARN (attach to the node role). |
| `credential_secret_arns` | upstream → SM ARN map. |
| `cache_repository_prefixes` | upstream → ECR prefix map. |
| `pull_through_cache_rules` | upstream → {prefix, upstream_url} map. |

[ptc]: https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html
[adr]: ../../../docs/research/canonical-prod-nat-elimination.md
