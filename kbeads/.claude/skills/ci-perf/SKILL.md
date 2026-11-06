---
name: ci-perf
description: >
  CI pipeline performance monitoring, Playwright test analysis, and app log
  correlation for the Acme monorepo. Use when investigating test failures,
  pipeline slowness, resource bottlenecks, or running ad-hoc e2e tests.
allowed-tools: "Bash,Read,Grep,Glob,WebFetch"
version: "1.0.0"
author: "matthewbaker"
---

# CI Pipeline Performance Monitoring

Observability toolkit for the Acme monorepo CI pipeline. Covers VictoriaMetrics metrics,
Playwright test analysis, app log collection, and ad-hoc test execution via debug pods.

## Quick Reference

| What | Where |
|------|-------|
| Grafana | `https://grafana.app.devops.acme.example` |
| VM Write (remote-write) | `https://vm-write.app.devops.acme.example` |
| VM Query API | vmselect pods in `observability` namespace on devops cluster |
| E2e cluster | `america-e2e-eks` (us-east-1) |
| Devops cluster | `america-devops-eks` (us-east-1) |
| Monorepo | `~/book/monorepo` (gitlab.com/example-org/monorepo) |
| Helm charts | `~/book/acme-helm-chart` (gitlab.com/example-org/acme-helm-chart) |
| E2e runner chart | `~/book/acme-helm-chart/charts/e2e-runner/` |
| Observability chart | `~/book/acme-helm-chart/charts/observability/` |
| Pipeline config | `~/book/monorepo/cicd/gitlab/` |

## Resources

| Resource | Content |
|----------|---------|
| [VICTORIAMETRICS.md](resources/VICTORIAMETRICS.md) | VictoriaMetrics architecture, PromQL queries, dashboard creation |
| [PLAYWRIGHT.md](resources/PLAYWRIGHT.md) | Test structure, reporting, shard weights, quarantine |
| [DEBUG_POD.md](resources/DEBUG_POD.md) | Running individual tests via the e2e debug pod |
| [APP_LOGS.md](resources/APP_LOGS.md) | App log collection, correlation with test failures |
| [PIPELINE.md](resources/PIPELINE.md) | Pipeline stages, timing breakdown, critical path |
| [INFRASTRUCTURE.md](resources/INFRASTRUCTURE.md) | Cluster configs, node groups, RDS, resource limits |
