# Running Tests via Debug Pod

The e2e-runner chart includes a debug pod for interactive Playwright test execution.

## Prerequisites

- kubectl access to `america-e2e-eks` cluster
- An active MR namespace (deployed via deploy-mr)
- Debug pod enabled in helm values (`debugPod.enabled: true` -- on by default in e2e.yaml)

## Finding the Debug Pod

```bash
# Switch to e2e cluster
kubectl config use-context america-e2e-eks

# List debug pods across MR namespaces
kubectl get pods -A -l app.kubernetes.io/component=debug | grep e2e-runner

# Or in a specific MR namespace
kubectl get pods -n mr-<branch-slug> | grep debug
```

## Connecting

```bash
# Interactive shell
kubectl exec -it <release>-e2e-runner-debug -n <namespace> -- bash

# Or with the namespace directly
kubectl exec -it -n mr-perf-ci-test-acceleration deploy/e2e-runner-debug -- bash
```

## Running Tests

Inside the debug pod:

```bash
cd /tests/e2e

# Run ALL tests (270 total via bun discovery)
bun run test --workers=3

# Run a specific spec file
bun run test specs/001-login/login.spec.ts

# Run tests matching a grep pattern
bun run test --grep "upload document"

# Run a specific directory
bun run test specs/030-etmf/

# Run with single worker for debugging
bun run test --workers=1 specs/060-isf/01-upload-document.spec.ts

# Run with retries
bun run test --retries=2 specs/070-coverage/

# Run with full trace/video/screenshot (override E2E_FAST)
E2E_FAST=false bun run test --workers=1 specs/001-login/login.spec.ts

# Run just the quarantined tests
bun run test $(cat quarantine.json | jq -r '.[]' | tr '\n' ' ')
```

## Environment Variables

The debug pod has these pre-configured (from helm values):

| Variable | E2E Value |
|----------|-----------|
| `SPONSOR_URL` / `UNITY_URL` | `https://unity.acme.e2e.dev.acme.example` |
| `SITE_URL` / `HARMONY_URL` | `https://harmony.acme.e2e.dev.acme.example` |
| `SYSADMIN_API` | `https://unity.acme.e2e.dev.acme.example/sysadmin/v1/sysAdmin/` |
| `USERNAME` | `piadmin` |
| `PASSWORD` | `TestAdmin123` |
| `ENV` | `cloud` |
| `LOG_LEVEL` | `info` |

For MR-specific deploys, URLs point to the MR namespace endpoints instead.

## Correlating with App Logs

Run tests in one terminal, tail app logs in another:

```bash
# Terminal 1: tail sponsor-api logs
kubectl logs -f -n <namespace> -l app=sponsor-api --all-containers --since=5m

# Terminal 2: tail site-api logs
kubectl logs -f -n <namespace> -l app=site-api --all-containers --since=5m

# Terminal 3: run a specific test
kubectl exec -it <debug-pod> -n <namespace> -- bash -c "cd /tests/e2e && bun run test --workers=1 specs/060-isf/01-upload-document.spec.ts"
```

This gives real-time correlation between test actions and server-side behavior.

## Collecting Reports

```bash
# Copy test results out of the pod
kubectl cp <namespace>/<debug-pod>:/tests/e2e/reports ./local-reports/

# Or just the JUnit XML
kubectl cp <namespace>/<debug-pod>:/tests/e2e/reports/playwright-junit.xml ./junit.xml
```

## Pod Specs

| Setting | Value |
|---------|-------|
| Image | `acme-playwright:latest` (same as CI) |
| CPU | 1000m request / 4000m limit |
| Memory | 2Gi request / 8Gi limit |
| User | root (UID 0, required for bun) |
| Node | amd64 only |
| Volumes | emptyDir for reports and test-results |

## Helm Chart Location

```
~/book/acme-helm-chart/charts/e2e-runner/
  templates/debug-pod.yaml    # Pod template
  templates/job.yaml          # CI job template
  values.yaml                 # Defaults
  values/e2e.yaml             # E2E overrides (debug enabled)
```

## Tips

- **bun vs npx**: Use `bun run test` (not `npx playwright test`) -- bun discovers all 270 tests vs 157 with npx
- **Root user**: Pod runs as root because bun binary is owned by root
- **Image freshness**: `imagePullPolicy: Always` in e2e, so restarting the pod gets latest image
- **Resource headroom**: 4 CPU / 8Gi is plenty for 1-3 workers; use `--workers=6` only if load-testing
