# Kubernetes Deployment

This directory contains Kubernetes manifests and scripts for deploying coopmux with on-demand session pod creation.

## Files

- **`k8s-mux.yaml`**: Deployment, Service, and RBAC for coopmux server
- **`k8s-launch.sh`**: Pod creation script called by coopmux launch API
- **`k8s-standalone.yaml`**: Example standalone coop pod (no mux)

## Quick Start

```bash
# Pull images from GHCR
docker pull <registry>/gardenio/coop:coopmux
docker pull <registry>/gardenio/coop:claude

# Load into cluster (kind/k3d)
kind load docker-image <registry>/gardenio/coop:coopmux <registry>/gardenio/coop:claude --name <cluster-name>
# or
k3d image import <registry>/gardenio/coop:coopmux <registry>/gardenio/coop:claude --cluster <cluster-name>

# Deploy
kubectl apply -f deploy/k8s-mux.yaml

# Access dashboard
kubectl port-forward svc/coopmux -n coop 9800:9800
# Open http://localhost:9800/mux
```

## Launch Script (`k8s-launch.sh`)

The `k8s-launch.sh` script creates session pods dynamically when the launch API is called. It supports environment variable customization for git cloning and working directory configuration.

### Environment Variables

#### Required (set by coopmux deployment)

- **`POD_NAMESPACE`**: Kubernetes namespace (injected via downward API)
- **`COOP_SESSION_IMAGE`**: OCI image for session pods (e.g., `<registry>/gardenio/coop:claude`)

#### Optional (set via launch API)

- **`GIT_REPO`**: Git repository URL
  - If set, triggers git-clone init container
  - Clones into `/workspace/repo` by default
  - Example: `https://github.com/user/myproject`

- **`GIT_BRANCH`**: Git branch to checkout
  - Default: `main`
  - Only used if `GIT_REPO` is set

- **`WORKING_DIR`**: Working directory for coop session
  - Default: `/workspace`
  - Session starts in this directory
  - If using git clone, set to `/workspace/repo` to start in the cloned repo

#### Injected by coopmux

- **`COOP_MUX_URL`**: Mux service URL (uses K8s DNS)
- **`COOP_MUX_TOKEN`**: Auth token for session registration
- **`ANTHROPIC_API_KEY`**, **`CLAUDE_CODE_OAUTH_TOKEN`**: Credentials (if healthy accounts exist in broker)

### Pod Spec Features

The generated pod includes:

1. **Init Container (conditional)**
   - Only created if `GIT_REPO` is set
   - Uses `alpine/git:latest` image
   - Clones repository with `--filter=blob:none` (blobless partial clone: full
     history + all refs, file contents fetched on-demand — not shallow)
   - Checks out specified branch

2. **Workspace Volume**
   - `emptyDir` volume mounted at `/workspace`
   - Shared between init container (git clone) and main container (coop)
   - Ephemeral: deleted when pod terminates

3. **Working Directory**
   - Configurable via `WORKING_DIR` env var
   - Session starts in this directory

4. **Credential Fallback**
   - Prefers credentials from coopmux broker (injected as env)
   - Falls back to K8s secret `anthropic-credentials` if broker has no healthy accounts

### Usage Examples

#### Launch with Git Clone

Via mux dashboard:
1. Click "New Session" in sidebar
2. Select "Git Clone" preset
3. Fill in:
   - `GIT_REPO`: `https://github.com/myorg/myrepo`
   - `GIT_BRANCH`: `main` (or your branch)
   - `WORKING_DIR`: `/workspace/repo`
4. Click "Launch Session"

Via API:
```bash
curl -X POST http://coopmux:9800/api/v1/sessions/launch \
  -H "Content-Type: application/json" \
  -d '{
    "env": {
      "GIT_REPO": "https://github.com/myorg/myrepo",
      "GIT_BRANCH": "feature-x",
      "WORKING_DIR": "/workspace/repo"
    }
  }'
```

#### Launch with Custom Working Directory (no git)

```bash
curl -X POST http://coopmux:9800/api/v1/sessions/launch \
  -H "Content-Type: application/json" \
  -d '{
    "env": {
      "WORKING_DIR": "/app"
    }
  }'
```

#### Launch with Defaults (no env vars)

```bash
curl -X POST http://coopmux:9800/api/v1/sessions/launch
```

Creates a session in `/workspace` with no git clone.

### Credential Configuration

#### Controller-Managed (Recommended)

When deployed with the gardenio controller, credentials are synced automatically:

1. Define account beads with provider and API key
2. Controller watches account bead events and pushes credentials to coopmux via `POST /api/v1/credentials/set`
3. Coopmux broker distributes credentials to sessions with pool load balancing

No manual credential configuration is needed — the controller handles the full lifecycle.

#### Kubernetes Secret (Standalone Fallback)

For standalone coopmux deployments without the gardenio controller:

```bash
kubectl create secret generic anthropic-credentials -n coop \
  --from-literal=api-key="$ANTHROPIC_API_KEY" \
  --from-literal=oauth-token="$CLAUDE_CODE_OAUTH_TOKEN"
```

Session pods will reference this secret if no broker credentials are available.

> **Note**: The `--credential-config` static JSON file is deprecated and will be removed in a future release.

### Troubleshooting

#### Check init container logs (git clone)

```bash
POD_NAME=$(kubectl get pods -n coop -l app=coop-session --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}')
kubectl logs -n coop $POD_NAME -c git-clone
```

#### Verify git clone succeeded

```bash
kubectl exec -n coop $POD_NAME -- ls -la /workspace/repo
```

#### Check session startup

```bash
kubectl logs -n coop $POD_NAME -c coop
```

#### Inspect environment variables

```bash
kubectl get pod -n coop $POD_NAME -o yaml | grep -A 20 'env:'
```

## RBAC

The coopmux pod requires permissions to create and manage session pods:

- **ServiceAccount**: `coopmux`
- **Role**: `coopmux-pod-manager`
  - Resources: `pods`
  - Verbs: `create`, `delete`, `get`, `list`, `watch`

Session pods use a minimal service account (`coop-session`) with no special permissions.

## See Also

- [Coopmux README](../crates/mux/README.md) — Launch API documentation
