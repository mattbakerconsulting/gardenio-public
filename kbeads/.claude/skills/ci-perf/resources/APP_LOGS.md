# App Log Collection and Correlation

## Current State: Major Gap

There is **no structured app log collection during e2e test runs** in CI. The deploy phase captures logs
only on failure. Once e2e starts, app pod logs are not collected anywhere.

This means when tests fail with 503/500 errors, the root cause is invisible from CI artifacts alone.

## What Exists

### Deploy Phase (eks/template.yml)

During `helm upgrade --install`, a background watcher:
- Polls pod status every 15s
- Captures setup job logs to `/tmp/setup-job-logs.txt`
- On deploy failure: comprehensive diagnostic dump (pods, events, logs from setup jobs)

**Not captured:** App pod logs after successful deploy.

### VictoriaLogs (Centralized)

VictoriaLogs (v1.43.1) runs in the devops cluster with a vlogs-collector DaemonSet (11 pods).
Endpoint: `victoria-logs.app.devops.acme.example`

This collects all container logs from devops cluster nodes. **E2e cluster log forwarding status unknown** --
check if the e2e observability chart includes a vlogs-collector.

### OTEL Traces

OTEL Collector in e2e forwards traces to devops at `otel-collector.app.devops.acme.example`.
VictoriaTraces (v0.5.0) stores traces. Check if app services emit OTEL spans.

## Manual Log Collection

### During a Test Run

```bash
# Tail all sponsor-api logs (all pods, all containers)
kubectl logs -f -n <namespace> -l app=sponsor-api --all-containers --since=1m --timestamps

# Tail site-api logs
kubectl logs -f -n <namespace> -l app=site-api --all-containers --since=1m --timestamps

# Tail everything in the namespace
kubectl logs -f -n <namespace> --all-containers --since=5m --timestamps -l 'app in (sponsor-api,site-api,sponsor-connect,site-connect)'

# Save logs to file with timestamps
kubectl logs -n <namespace> -l app=sponsor-api --all-containers --since=30m --timestamps > sponsor-api.log
```

### Using stern (if installed)

```bash
# Install stern
brew install stern  # or: go install github.com/stern/stern@latest

# Tail all pods in namespace with color-coded output
stern -n <namespace> ".*" --since 5m

# Filter to specific pods
stern -n <namespace> "sponsor-api|site-api" --since 5m

# With regex on log content
stern -n <namespace> ".*" --include "ERROR|WARN|500|503" --since 10m
```

## Correlating Test Failures with App Logs

### Step-by-step

1. **Identify the failure time**: Parse JUnit XML for the test start/end time
2. **Get the namespace**: From the pipeline job or MR branch slug (`mr-<branch>`)
3. **Pull logs for that window**:
   ```bash
   kubectl logs -n mr-<branch> -l app=sponsor-api --since-time="2026-02-24T20:00:00Z" --timestamps
   ```
4. **Search for error patterns**:
   ```bash
   kubectl logs -n mr-<branch> -l app=site-api --since=30m | grep -i "error\|exception\|503\|500\|traceback"
   ```

### Common Error Patterns

| Test Symptom | App Log Pattern | Likely Cause |
|---|---|---|
| Timeout waiting for element | No errors in logs | Frontend rendering issue or slow Kafka sync |
| 503 Service Unavailable | `Connection refused` or no logs | Pod restarting or not ready |
| 500 Internal Server Error | Python traceback | Backend bug |
| 401 Unauthorized | `JWKS validation failed` | Token expiry during long parallel run |
| 409 Conflict | `unique constraint violation` | Slug collision from parallel test data |

## Proposed Improvements

### Priority 1: CI Log Sidecar

Add a sidecar or init container to the e2e job that:
1. Starts `stern` or `kubectl logs -f` for app pods at test start
2. Writes to a file in the reports volume
3. Uploads as CI artifact alongside JUnit XML

### Priority 2: Structured Logging

Ensure app services emit structured JSON logs with request ID / trace ID that can be
correlated with Playwright network requests.

### Priority 3: VictoriaLogs for E2E

Verify/add vlogs-collector to the e2e cluster so all app logs are searchable via the
VictoriaLogs API after a test run completes.

### Priority 4: Test-Level Log Capture

Extend the Playwright test framework to:
- Record the timestamp range for each test
- After failure, query app logs for that time range via kubectl or VictoriaLogs API
- Attach relevant log snippets to the test report
