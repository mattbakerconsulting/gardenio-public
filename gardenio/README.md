# Gardenio

K8s agent controller and coopmux for beads automation — extracted from Gastown.

## Structure

```
controller/              # Go module — K8s agent controller
├── cmd/controller/      # Entry point
├── internal/
│   ├── beadsapi/        # HTTP client to beads daemon API
│   ├── config/          # Env var parsing
│   ├── podmanager/      # Pod spec construction & CRUD
│   ├── reconciler/      # Periodic desired-vs-actual sync
│   ├── statusreporter/  # Pod phase → bead state updates
│   └── subscriber/      # SSE/NATS event listener
├── Dockerfile
└── Makefile
helm/gardenio/            # Helm chart
├── templates/
│   ├── controller/      # Controller deployment + RBAC
│   └── coopmux/         # Coopmux deployment + service
├── Chart.yaml
└── values.yaml
images/
├── agent/               # Agent pod image + entrypoint
└── slack-bridge/        # Slack bridge Dockerfile
Makefile                  # Top-level build
README.md
```

## Prerequisites

Gardenio assumes a **beads daemon** (`bd-daemon`) is already deployed and reachable. Configure the connection via helm values:

```yaml
daemon:
  host: bd-daemon.beads.svc.cluster.local
  httpPort: 9080
  tokenSecret: bd-daemon-token
```

## Quick Start

```bash
# Build controller binary
make build

# Run tests
make test

# Build Docker image
make image

# Render helm templates (dry-run)
make helm-template

# Package helm chart
make helm-package
```

## Deployment

```bash
helm install gardenio helm/gardenio/ \
  --namespace agents \
  --set daemon.host=bd-daemon.beads.svc.cluster.local \
  --set daemon.tokenSecret=bd-daemon-token \
  --set agents.enabled=true \
  --set coopmux.enabled=true
```
