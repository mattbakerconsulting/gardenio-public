#!/usr/bin/env bash
# setup-perimeter-guard.sh — Create the perimeter-guard scheduled crew.
# Idempotent: checks for existing schedule before creating.
#
# The perimeter-guard crew spawns every 8 hours to audit security posture:
#   - Analyzes access logs via Mezmo for suspicious patterns
#   - Verifies K8s services are not exposed to the internet
#   - Checks NetworkPolicies, IngressRoutes, and auth middleware
#   - Reports findings and hardening recommendations
#
# Usage: ./scripts/setup-perimeter-guard.sh
#    or: KD_HOST=http://beads.gardenio-e2e:8080 ./scripts/setup-perimeter-guard.sh
set -euo pipefail

if ! command -v kd &>/dev/null; then
  echo "Error: kd not found in PATH" >&2
  exit 1
fi

ROLE_NAME="perimeter-guard"
SCHEDULE_TITLE="Perimeter Guard: security posture audit"
LABEL="project:gardenio"

# ── Step 1: Create role bead (idempotent) ────────────────────────────────────
if kd list --type=role --all-projects 2>/dev/null | grep -qF "$ROLE_NAME"; then
  echo "[skip] Role '$ROLE_NAME' already exists"
  ROLE_ID=$(kd list --type=role --all-projects 2>/dev/null | grep "$ROLE_NAME" | awk '{print $1}')
else
  echo "Creating role bead '$ROLE_NAME'..."
  ROLE_ID=$(kd create "$ROLE_NAME" --type=role \
    2>&1 | grep -oP 'kd-\w+' | head -1)

  if [ -z "$ROLE_ID" ]; then
    echo "Error: Failed to create role bead" >&2
    exit 1
  fi
  echo "[created] Role '$ROLE_NAME' → $ROLE_ID"
fi

# ── Step 2: Create schedule bead (idempotent) ────────────────────────────────
if kd list --type=schedule -l "$LABEL" 2>/dev/null | grep -qF "Perimeter Guard"; then
  echo "[skip] Perimeter Guard schedule already exists"
  existing=$(kd list --type=schedule -l "$LABEL" 2>/dev/null | grep "Perimeter Guard" | awk '{print $1}')
  echo "       ID: $existing"
  echo "       Use 'kd show $existing' to view details"
  exit 0
fi

# The prompt instructs the spawned agent on what to audit and how to report.
# It is intentionally detailed so the agent can operate autonomously.
read -r -d '' PROMPT << 'PROMPT_EOF' || true
You are the **perimeter-guard** crew. Your mission: audit gardenio's security
posture, check access logs for threats, verify no services are internet-exposed
without auth, and report hardening recommendations.

## Phase 1 — Access Log Analysis (Mezmo)

Use Mezmo MCP tools to analyze the last 8 hours of logs.

### 1.1 Suspicious access patterns
- Use `analyze_logs_for_root_cause_relative_time` with `since: "8 hours"` and
  query `level:error (401 OR 403 OR "unauthorized" OR "forbidden")` to find
  failed auth attempts.
- Use `group_logs_by_field` to group by `host` and `app` with query
  `level:error` to find which services are seeing the most errors.
- Use `get_log_histogram` over the last 8 hours with query
  `level:error (401 OR 403)` to spot spikes in auth failures.

### 1.2 External access attempts
- Search for requests to internal-only endpoints:
  query `("/debug" OR "/metrics" OR "/healthz" OR "/admin" OR "/internal")`
- Check for any access to postgres, NATS, or other backend services that
  should never receive external traffic.

### 1.3 Anomalous traffic
- Use `group_logs_by_field` grouped by `host` to see if any unexpected
  hosts are receiving traffic.
- Look for unusually high request volumes from single sources.

## Phase 2 — K8s Network Security Audit

Use kubectl to inspect the gardenio namespace.

### 2.1 Service exposure
```bash
# List all services — flag any LoadBalancer or NodePort types
kubectl get svc -n gardenio -o custom-columns='NAME:.metadata.name,TYPE:.spec.type,CLUSTER-IP:.spec.clusterIP,EXTERNAL-IP:.status.loadBalancer.ingress[*].ip,PORTS:.spec.ports[*].port'
```
**Expected**: All services should be ClusterIP. Any LoadBalancer or NodePort
is a finding unless explicitly documented.

### 2.2 IngressRoute auth verification
```bash
# List all IngressRoutes and their middlewares
kubectl get ingressroute -n gardenio -o jsonpath='{range .items[*]}{"ROUTE: "}{.metadata.name}{"\n"}{range .spec.routes[*]}{"  match: "}{.match}{"\n"}{range .middlewares[*]}{"  middleware: "}{.name}{"\n"}{end}{end}{"\n"}{end}'
```
**Check**: Every public-facing IngressRoute should have either:
- `forwardauth` middleware (Keycloak/OAuth2 Proxy), OR
- `ipwhitelist` middleware (IP allowlist), OR
- Both

Routes without auth middleware that serve user data are critical findings.
Webhook receiver routes (e.g., slack-bridge for Slack Events API, gitlab-bridge
for GitLab webhooks, jira-bridge for JIRA webhooks) may legitimately skip
forwardauth since they use provider-specific signature verification — note
these but don't flag them as critical.

### 2.3 NetworkPolicy coverage
```bash
# Verify default-deny exists and per-service policies are in place
kubectl get networkpolicy -n gardenio -o custom-columns='NAME:.metadata.name,POD-SELECTOR:.spec.podSelector'
```
**Check**: There must be a `default-deny-ingress` policy. Every service with
an IngressRoute should have an explicit allow policy.

### 2.4 Pod security contexts
```bash
# Check for pods running as root or without security constraints
kubectl get pods -n gardenio -o jsonpath='{range .items[*]}{"POD: "}{.metadata.name}{"\n"}{range .spec.containers[*]}{"  container: "}{.name}{" runAsNonRoot="}{.securityContext.runAsNonRoot}{" readOnlyRootFilesystem="}{.securityContext.readOnlyRootFilesystem}{" runAsUser="}{.securityContext.runAsUser}{"\n"}{end}{end}'
```
**Check**: Containers should run as non-root where possible. Flag any running
as root without documented justification.

### 2.5 TLS verification
```bash
# Verify TLS secrets exist and are not expired
kubectl get ingressroute -n gardenio -o jsonpath='{range .items[*]}{.metadata.name}{" tls="}{.spec.tls.secretName}{"\n"}{end}'
kubectl get secret -n gardenio -o custom-columns='NAME:.metadata.name,TYPE:.type' | grep tls
```

### 2.6 RBAC review
```bash
# Check for overly permissive cluster roles bound to gardenio service accounts
kubectl get rolebinding,clusterrolebinding -n gardenio -o custom-columns='NAME:.metadata.name,ROLE:.roleRef.name,SUBJECTS:.subjects[*].name' 2>/dev/null | head -30
```

## Phase 3 — Report

Create a summary bead with your findings:

```bash
kd create "Perimeter Guard Report: $(date +%Y-%m-%d %H:%M UTC)" \
  -t task -l project:gardenio -l perimeter-guard -l security-audit \
  -p <PRIORITY> \
  -d "<your findings>"
```

Priority guidelines:
- **P1** (priority=3): Critical — services exposed without auth, default-deny
  missing, active exploitation detected in logs
- **P2** (priority=2): High — auth bypass possible, missing NetworkPolicies,
  pods running as root unnecessarily, TLS issues
- **P3** (priority=1): Medium — informational findings, hardening suggestions,
  log anomalies worth monitoring
- **P0** (priority=0): Clean — no findings, all checks passed

Structure the report as:
1. **Summary**: One-line overall status (CLEAN / FINDINGS / CRITICAL)
2. **Access Log Findings**: What Mezmo showed — anomalies, auth failures, traffic patterns
3. **Network Security Findings**: K8s audit results — exposure, auth, policies
4. **Recommendations**: Specific, actionable hardening steps ranked by impact
5. **Changes Since Last Audit**: Note any new services, removed policies, or config drift

If you find critical issues (P1), also post a comment on any open
perimeter-guard beads to ensure visibility.

After creating the report bead, run `gb stop` to signal completion.
PROMPT_EOF

echo "Creating perimeter-guard schedule..."
SCHED_ID=$(kd create "$SCHEDULE_TITLE" --type=schedule \
  -l "$LABEL" \
  -f "cron=0 */8 * * *" \
  -f "enabled=true" \
  -f "project=gardenio" \
  -f "role=$ROLE_NAME" \
  -f "name=perimeter-guard" \
  -f "prompt=$PROMPT" \
  -f "timezone=UTC" \
  -f "timeout=20m" \
  -f "max_concurrent=1" \
  -f "retry_on_failure=false" \
  -f "max_retries=3" \
  2>&1 | grep -oP 'kd-\w+' | head -1)

if [ -z "$SCHED_ID" ]; then
  echo "Error: Failed to create schedule bead" >&2
  exit 1
fi

echo "[created] $SCHEDULE_TITLE"
echo "          Schedule ID: $SCHED_ID"
echo "          Role ID:     $ROLE_ID"
echo "          Cron: 0 */8 * * * (every 8 hours at :00 UTC)"
echo "          Role: $ROLE_NAME"
echo "          Name: perimeter-guard"
echo "          Timeout: 20m"
echo ""
echo "Manage with:"
echo "  kd show $SCHED_ID              # view schedule details"
echo "  kd show $ROLE_ID               # view role config"
echo "  kd update $SCHED_ID -f 'enabled=false'  # disable"
echo "  kd update $SCHED_ID -f 'enabled=true'   # re-enable"
echo "  kd update $SCHED_ID -f 'cron=0 6,14,22 * * *'  # change schedule"
