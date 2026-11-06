# Perimeter Guard Crew

Automated security posture auditing for gardenio services. Spawns every 8 hours
to check access logs, verify network isolation, and recommend hardening steps.

## What It Does

Every 8 hours (00:00, 08:00, 16:00 UTC), the perimeter-guard crew agent:

1. **Analyzes access logs** (via Mezmo) — scans for failed auth attempts,
   requests to internal endpoints, traffic anomalies, and error spikes
2. **Audits K8s network security** — checks Service types, IngressRoute auth
   middleware, NetworkPolicy coverage, pod security contexts, TLS, and RBAC
3. **Creates a report bead** — tagged `perimeter-guard` + `security-audit`,
   with prioritized findings and actionable recommendations

## Setup

```bash
./scripts/setup-perimeter-guard.sh
```

The script is idempotent — safe to run multiple times.

## Viewing Reports

```bash
# List recent perimeter guard reports
kd list -l perimeter-guard -l security-audit

# Show the latest report
kd show <report-id>
```

## Managing the Schedule

```bash
# Find the schedule bead
kd list --type=schedule -l project:gardenio | grep "Perimeter Guard"

# Disable temporarily
kd update <schedule-id> -f 'enabled=false'

# Re-enable
kd update <schedule-id> -f 'enabled=true'

# Change frequency (e.g., every 6 hours)
kd update <schedule-id> -f 'cron=0 */6 * * *'

# Change timeout (default: 20m)
kd update <schedule-id> -f 'timeout=30m'
```

## What Gets Checked

### Access Logs (Mezmo)

| Check | What It Catches |
|---|---|
| Auth failures (401/403) | Brute force, credential stuffing, expired tokens |
| Internal endpoint access | Requests to `/debug`, `/metrics`, `/admin`, `/internal` |
| Per-host error distribution | Which services are under stress or attack |
| Auth failure spikes | Coordinated attacks, misconfigured clients |
| Unexpected host traffic | Services receiving traffic they shouldn't |

### K8s Network Security

| Check | Expected State | Critical If |
|---|---|---|
| Service types | All ClusterIP | LoadBalancer or NodePort found |
| IngressRoute auth | forwardAuth or ipWhitelist on every route | Route without auth middleware serves user data |
| NetworkPolicies | default-deny + per-service allow | default-deny missing |
| Pod security contexts | Non-root, read-only rootfs | Pods running as root without justification |
| TLS | All IngressRoutes reference TLS secret | Plaintext routes |
| RBAC | Least-privilege bindings | Overly permissive cluster roles |

### Known Exceptions

Some services legitimately skip forwardAuth because they use provider-specific
verification:

- **slack-bridge** — Validates Slack request signatures (`X-Slack-Signature`)
- **gitlab-bridge** — Validates GitLab webhook tokens
- **jira-bridge** — Validates JIRA webhook signatures
- **beads CI route** — Uses bearer token auth enforced by beads daemon

These are noted in reports but not flagged as critical.

## Report Priority Levels

| Priority | Meaning | Example |
|---|---|---|
| P1 (3) | Critical — action required now | Service exposed without auth, active exploitation |
| P2 (2) | High — fix soon | Missing NetworkPolicy, unnecessary root pods |
| P3 (1) | Medium — monitor/improve | Informational findings, hardening suggestions |
| P0 (0) | Clean — all checks passed | No findings |

## Architecture

```text
Scheduler (controller)
  │
  ├─ every 8h ──▶ spawns perimeter-guard agent pod
  │                (role bead: perimeter-guard)
  ▼
Agent Pod
  ├─ Phase 1: Mezmo MCP ──▶ access log analysis
  ├─ Phase 2: kubectl ──▶ K8s security audit
  └─ Phase 3: kd create ──▶ report bead
```

The setup script creates two beads:

1. **Role bead** (`perimeter-guard`): Identifies the agent role. Uses default
   model (Opus) and resource limits, same as all other gardenio agents.
2. **Schedule bead**: Defines the 8-hour cron, references the role, and
   contains the full audit prompt.

The crew uses the standard gardenio agent image — no custom image needed. It
accesses Mezmo via MCP tools (configured in the agent environment) and K8s
via the agent service account's RBAC permissions.

## Tuning the Role

```bash
# Increase resources if audits are timing out
kd update <role-id> -f 'cpu_request=1000m' -f 'memory_request=1Gi'
```
