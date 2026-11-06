# VictoriaMetrics Monitoring

## Architecture

The devops cluster runs a full VictoriaMetrics cluster (v1.106.1) in the `observability` namespace:

```
                     Grafana (grafana.app.devops.acme.example)
                         |
                    vmselect (2x, 512Mi-2Gi)
                         |
                    vmstorage (5x, 50Gi each, RF=2, 14d retention)
                         ^
              +----------+----------+
              |                     |
         vminsert (5x)         vminsert (5x)
              ^                     ^
              |                     |
         vmagent (local)    vm-write.app.devops.acme.example
         (devops scrape)         (e2e remote-write)
```

- **E2e cluster** runs lightweight Prometheus (2 replicas, 2h retention) that remote-writes to devops VMCluster
- E2e metrics are labeled `cluster: "e2e"`
- VMAgent uses `selectAllByDefault: true` -- auto-discovers all VMServiceScrape/VMPodScrape resources

## Key Endpoints

| Endpoint | Purpose |
|----------|---------|
| `grafana.app.devops.acme.example` | Grafana dashboards |
| `vm-write.app.devops.acme.example/insert/0/prometheus/api/v1/write` | Remote-write receiver |
| `victoria-traces.app.devops.acme.example` | Trace ingestion |
| `victoria-logs.app.devops.acme.example` | Log ingestion |
| `otel-collector.app.devops.acme.example` | OTEL trace ingestion |

## Useful PromQL Queries

### CI Runner Pod Resources

```promql
# CPU usage of pods in gitlab-runner namespace (CI jobs)
sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="gitlab-runner"}[5m]))

# Memory usage of CI job pods
sum by (pod) (container_memory_working_set_bytes{namespace="gitlab-runner"})

# Network I/O for CI pods
sum by (pod) (rate(container_network_receive_bytes_total{namespace="gitlab-runner"}[5m]))
sum by (pod) (rate(container_network_transmit_bytes_total{namespace="gitlab-runner"}[5m]))

# Disk I/O for CI pods
sum by (pod) (rate(container_fs_reads_bytes_total{namespace="gitlab-runner"}[5m]))
sum by (pod) (rate(container_fs_writes_bytes_total{namespace="gitlab-runner"}[5m]))
```

### E2E Cluster App Pod Resources

```promql
# CPU by pod in an MR namespace (e2e cluster)
sum by (pod) (rate(container_cpu_usage_seconds_total{cluster="e2e", namespace=~"mr-.*"}[5m]))

# Memory by pod in MR namespace
sum by (pod) (container_memory_working_set_bytes{cluster="e2e", namespace=~"mr-.*"})

# OOMKill events
kube_pod_container_status_last_terminated_reason{reason="OOMKilled", cluster="e2e"}
```

### Node-Level Metrics

```promql
# Node CPU saturation (devops)
1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))

# Node memory pressure
1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)

# Disk pressure (e2e cluster)
1 - (node_filesystem_avail_bytes{cluster="e2e", mountpoint="/"} / node_filesystem_size_bytes{cluster="e2e", mountpoint="/"})
```

### GitLab Runner Metrics (if ServiceMonitor added)

```promql
# Active CI jobs
gitlab_runner_jobs{state="running"}

# Job processing rate
rate(gitlab_runner_jobs_total[5m])

# Runner errors
rate(gitlab_runner_errors_total[5m])
```

## Querying from CLI

```bash
# Switch to devops cluster
kubectl config use-context america-devops-eks

# Port-forward to vmselect for direct queries
kubectl -n observability port-forward svc/vmselect-vm 8481:8481 &

# Query via curl
curl -s 'http://localhost:8481/select/0/prometheus/api/v1/query?query=up' | jq .

# Range query (last hour)
curl -s 'http://localhost:8481/select/0/prometheus/api/v1/query_range?query=rate(container_cpu_usage_seconds_total{namespace="gitlab-runner"}[5m])&start='$(date -v-1H +%s)'&end='$(date +%s)'&step=30s' | jq .
```

## Monitoring Gaps (To Fix)

1. **No GitLab Runner ServiceMonitor** -- runner exposes metrics on port 9252 but no VMServiceScrape exists
2. **No CI pipeline dashboard** -- cAdvisor metrics exist but no dedicated Grafana dashboard
3. **Karpenter monitoring disabled** -- `karpenter.enabled: false` in devops values; dashboards ready but turned off
4. **vmselect near memory limit** -- 2 pods at ~2Gi each, limit is 2Gi; watch for OOMs under heavy query load

## Helm Chart Locations

| File | Purpose |
|------|---------|
| `charts/observability/Chart.yaml` | Chart definition |
| `charts/observability/values.yaml` | Base values |
| `charts/observability/values/devops.yaml` | Devops cluster overrides |
| `charts/observability/values/e2e.yaml` | E2e cluster overrides (Prometheus remote-write) |
| `charts/observability/templates/vm-grafana-datasources.yaml` | VM datasource for Grafana |
| `charts/observability/templates/vm-remote-write-ingressroute.yaml` | External write endpoint |
