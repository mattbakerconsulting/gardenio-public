# Cluster Infrastructure

## E2E Cluster: america-e2e-eks

| Property | Value |
|----------|-------|
| Region | us-east-1 |
| VPC CIDR | 10.30.0.0/16 |
| AWS Account | 234567890123 |
| K8s Version | EKS 1.32 |
| Domain | e2e.dev.acme.example / e2e.example.com |

### Node Groups

| Group | Arch | Capacity | Instances | Min/Des/Max | EBS |
|-------|------|----------|-----------|-------------|-----|
| general-x86 | x86_64 | SPOT | m5/c5 large-2xl | 2/2/60 | 50Gi |
| general-arm64 | ARM64 | SPOT | m6g/c6g large-2xl | 2/2/60 | 50Gi |
| dedicated-x86 | x86_64 | ON_DEMAND | r5.2xlarge | 1/1/1 | 200Gi |
| dedicated-arm64 | ARM64 | ON_DEMAND | r6g.2xlarge | 2/2/2 | 200Gi |
| persistence-az{1,2,3} | x86_64 | ON_DEMAND | m5/r5 xl-2xl | 0/1/3 | 100Gi |

- General pools use **SPOT** with large max (60) for burst
- Dedicated nodes have `cicd-dedicated` taint
- Karpenter infra provisioned (IAM, SQS, EventBridge) but **helm chart disabled**
- Cluster autoscaler active via tenant-system chart

### Kubelet Config
- maxPods: 110, podsPerCore: 10
- System reserved: 500Mi mem, 100m CPU
- Kube reserved: 500Mi mem, 100m CPU
- Soft eviction at 20% free memory, hard at 10%
- High image pull: registryPullQPS=50, registryBurst=100

## Devops Cluster: america-devops-eks

Hosts CI runners, VictoriaMetrics, Grafana, and observability stack.

Key components:
- GitLab runners (gitlab-runner namespace)
- VictoriaMetrics cluster (observability namespace)
- Grafana (observability namespace)
- VictoriaLogs + vlogs-collector DaemonSet
- VictoriaTraces + OTEL Collector

## RDS Database (E2E)

| Property | Value |
|----------|-------|
| Cluster | america-e2e |
| Engine | Aurora PostgreSQL |
| Instance | db.t3.medium |
| Multi-AZ | Disabled |
| Instances | 1 (single writer) |
| Audit logging | Disabled |

## Application Resources (E2E)

From `charts/tenant/values/e2e.yaml`:

| Setting | Value | Rationale |
|---------|-------|-----------|
| defaultReplicas | 2 | Per service |
| dbMaxPoolSize | 5 | Per pod |
| dbMaxOverflow | 15 | Total capacity: 2 pods x 20 = 40 connections/service |
| VPA | Auto mode | Manages resource requests dynamically |
| CPU request | 10m | Intentionally low for VPA eviction |
| CPU limit | 1000m | |
| Memory request | 64-128Mi | Intentionally low for VPA |
| Memory limit | 2048Mi | |
| VPA max memory | 4Gi | |

### Startup Probes
- Initial delay: 20s
- Period: 3s, timeout: 3s
- Failure threshold: 20 (60s window total)

## E2E Runner Resources

| Setting | Default | E2E Override |
|---------|---------|-------------|
| CPU request/limit | 1000m/2000m | 2000m/4000m |
| Memory request/limit | 2Gi/4Gi | 4Gi/8Gi |
| Job TTL | 3600s | 7200s |
| Active deadline | none | 3600s |
| nodeSelector | none | amd64 only |
| securityContext | nonRoot/1000 | root/0 (bun needs root) |
| imagePullPolicy | IfNotPresent | Always |

## Infrastructure Services (E2E)

| Service | Configuration |
|---------|--------------|
| Kafka | Strimzi shared cluster, SCRAM-SHA-512 |
| Keycloak | External ECS: `auth.e2e.dev.acme.example` |
| OpenSearch | AWS managed: `america-e2e.us-east-1.es.amazonaws.com` |
| S3 | Unified bucket `acme-e2e-storage`, Pod Identity ABAC |
| Prometheus | 2 replicas, 2h retention, remote-write to devops VM |
| OTEL | Single collector -> devops |
| Dolt | Single instance, 50Gi PVC, S3 sync |

## Terraform Source

Infrastructure defined in:
- `~/book/site-deployment/terraform/new-modules/base/eks/main.tf` (EKS cluster)
- `~/book/site-deployment/terraform/live/e2e/regions/america/` (E2E environment)
- `~/book/site-deployment/terraform/new-modules/cluster/database/main.tf` (RDS)

## Test Data Restore

Pre-install helm hook (`charts/tenant/templates/test-data-restore-job.yaml`):
1. Downloads from `s3://acme-e2e-storage/public/backups/{sponsor,site}.sql`
2. DROP + CREATE both databases
3. Restore from SQL seeds (~800KB, schema + reference data only)
4. Resources: 100m CPU, 256Mi memory
5. Uses Pod Identity for S3 access with 3 retry attempts
