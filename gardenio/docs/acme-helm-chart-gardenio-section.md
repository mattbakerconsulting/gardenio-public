# Gardenio Chart Section for acme-helm-chart CLAUDE.md

> Add this section to acme-helm-chart/CLAUDE.md under "Core Charts"

## Gardenio Chart

**Chart**: `charts/gardenio/` — wrapper chart for the gardenio agent platform.

**Architecture**: This is a subchart wrapper that depends on the upstream gardenio OCI chart:
```yaml
dependencies:
  - name: gardenio
    version: ">=2026.0.0"
    repository: "oci://<registry>/gardenio/charts"
  - name: beads3d
    version: "2026.59.0"
    repository: "oci://<registry>/gardenio/charts"
    condition: beads3d.enabled
```

**Key Points**:
- **Fuzzy version constraint**: `helm dependency update` resolves the latest upstream chart matching `>=2026.0.0`
- **Subchart values prefix**: All upstream values must be nested under `gardenio:` prefix in `values/gardenio.yaml`
- **beads3d templates inlined**: Wrapper chart has custom beads3d templates because upstream lacks daemon token auth
- Do NOT pin exact upstream chart versions — the deploy job resolves latest automatically

**Values Files**:
- `values/gardenio.yaml` — production config (agents, bridges, NATS, etc.)
- `values/gardenio-e2e.yaml` — minimal E2E config (no agents/bridges, just controller + beads)

**Deploy Pipeline** (`.gitlab/ci/gardenio-deploy.yml`):
```bash
# Automatic: triggered by schedule, API, or web with GARDENIO_DEPLOY=1
# What it does:
#   1. helm registry login registry.gitlab.com  (REGISTRY_TOKEN)
#   2. helm dependency update        (resolves latest upstream chart)
#   3. helm upgrade --install gardenio ./ -n gardenio --values values/gardenio.yaml
```

**Trigger Methods**:
- **Schedule**: `GARDENIO_DEPLOY=1` in scheduled pipeline variables
- **Manual (UI)**: Build > Pipelines > Run pipeline, set `GARDENIO_DEPLOY=1`
- **API**: `glab api -X POST projects/:id/pipeline -f ref=main -f "variables[][key]=GARDENIO_DEPLOY" -f "variables[][value]=1"`
- **Auto on changes**: Chart changes on main trigger manual deploy option
- **From gardenio release**: GitLab CI triggers pipeline via API

**Manual Deploy (Emergency)**:
```bash
cd charts/gardenio
echo "$REGISTRY_TOKEN" | helm registry login registry.gitlab.com -u "$REGISTRY_USER" --password-stdin
helm dependency update
helm upgrade --install gardenio ./ -n gardenio --values values/gardenio.yaml --wait --timeout 5m
kubectl get pods -n gardenio
```
