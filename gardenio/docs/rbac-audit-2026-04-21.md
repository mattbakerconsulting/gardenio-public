# RBAC Least-Privilege Audit — 2026-04-21

**Scope**: every (Cluster)Role/(Cluster)RoleBinding defined by the gardenio Helm
chart (`gardenio/helm/gardenio/templates/**`).
**Method**: cross-mapped every rule against the caller code under
`gardenio/controller/**` and `gardenio/images/agent/**`, focusing on the
controller binary. Tests (`*_test.go`) were excluded from the call graph.
**Bead**: ka-0000004571. Companion: AUDIT-2026-04-18 (C1/C2/C3 already fixed).

## TL;DR

Posture is good. The earlier "agent SA is cluster-admin" (C1) regression is
gone — `agent-rbac.yaml` now ships an empty `rules: []` Role by default. The
remaining findings are **unused verbs/resources** on the controller's
namespaced `Role`, one **wrong verb** on ExternalSecret management (`update`
is granted, `delete` is required), and **two unavoidable cluster-scoped
escalation primitives** gated behind opt-in values (`clusterAdmin.enabled`,
`fleetManager.enabled`) that should be documented rather than "fixed".

No wildcards (`*`) anywhere. No `pods/exec|attach|portforward|proxy|eviction`,
no `impersonate`, no `nodes/proxy`, no `csr approve`, no `escalate` verb.

---

## Per-SA findings

Mapping of SA → bindings → actual-use (from `grep -rn …` on controller Go
sources, not tests). File references use `helm/gardenio/templates/` as the
root.

### 1. Controller SA (`gardenio-agents`)

Confusingly named — this SA is the **controller Deployment's** SA, not the
agent-pod SA (that's `coop.serviceAccountName`, see §2). Defined in
`controller/serviceaccount.yaml`. Bound to 1 namespaced Role and up to 3
ClusterRoles.

#### 1a. Namespaced Role — `controller/rbac.yaml`

| Resource | Granted verbs | Used verbs (caller) | Gap |
|---|---|---|---|
| `pods` | get, list, watch, create, delete, deletecollection | get, list, create, delete (`podmanager/manager.go`, `statusreporter/reporter.go`, `pvcgc/collector.go`) | **remove `watch`** (no informers), **remove `deletecollection`** (delete-by-name only) |
| `pods/log` | get | *(none — no `GetLogs(`, no `pods/log` subresource calls)* | **remove entire rule** |
| `persistentvolumeclaims` | get, list, create, delete | get, list, create, update, delete (`podmanager/manager.go` L358/L379/L400, `pvcgc/collector.go`) | **add `update`** (PVC Helm-adoption path at L400 is currently unauthorized on pre-existing PVCs) |
| `secrets` | get | get (`main.go`, `litellm_aliases.go`, `ratelimit_probe.go`); **create/delete** are required by `agenttoken.go` L83/L106 | current grant is insufficient for per-agent tokens — **add `create`, `delete`** when `perAgentTokens=true`, or pin by `resourceNames` (see §Recommendations) |
| `events` | get, list | *(none)* | **remove entire rule** |
| `services` | get, list, create, update, delete | *(none)* | **remove entire rule** |
| `deployments` (apps) | get, list, create, update, delete | *(none)* | **remove entire rule** |
| `leases` (coordination) | get, list, watch, create, update, patch, delete | leader-election LeaseLock in `main.go` L280 needs get, create, update | **trim to `get, create, update`** (plus optionally `watch` if `client-go` ≥1.26 uses it — current k8s.io/client-go does not) |
| `externalsecrets` (external-secrets.io) | get, list, create, **update** | get, list, create, **delete** (`secretreconciler/reconciler.go`) | **swap `update` → `delete`** |

**Surprising facts**
- `services` + `deployments` rules are dead — both predate a refactor and have
  zero callers in the controller. Token-rotator has its own SA with its own
  `deployments, statefulsets: patch` rule, so this is not load-bearing.
- `pods/log` is granted but the controller never streams logs; log access is
  a ops-only concern via `kubectl logs` with operator credentials.
- `watch` on pods sounds right ("must be a controller"), but this codebase
  polls via List every `PodSyncInterval` instead of using informers
  (`statusreporter/reporter.go` L253). Leaving the grant is cheap but
  least-privilege says remove it until a future informer lands.

#### 1b. ClusterRole — `controller/pv-gc-rbac.yaml`

| Resource | Granted | Used | Gap |
|---|---|---|---|
| `persistentvolumes` | get, list, patch, delete | list, patch, delete (`pvgc/collector.go` L62/L107/L138) | **remove `get`** (unused — collector only calls `List` then `Patch`/`Delete` by name; there's no standalone `Get`) |

**Posture note.** PVs are cluster-scoped; `resourceNames` can't pre-declare
dynamically-named PVs. The controller code filters by `spec.claimRef.namespace
∈ pvGCNamespaces`, but RBAC can't enforce that. This is the correct least-
privilege shape given the API.

#### 1c. ClusterRole — `controller/fleet-manager-rbac.yaml` (gated `fleetManager.enabled`)

| Resource | Granted | Used (by `cmd/gb/fleet.go`) | Gap |
|---|---|---|---|
| `namespaces` | get, list, create, patch, delete | create, delete (L161, L216) | **remove `get, list, patch`** unless pending/planned use |
| `serviceaccounts` | get, list, create, update, patch, delete | create (L239) | **trim to `create`** (at least by default; add `delete` behind a `gb fleet delete` migration flag) |
| `roles, rolebindings` (rbac.authorization.k8s.io) | get, list, create, update, patch, delete | create (L278, L307) | **trim to `create`** — same as above |
| `secrets` | get, create | create, get (L344/L347) | **OK** |
| `externalsecrets` (external-secrets.io) | get, list, create | create | **trim to `create`** (no `List`/`Get` calls in fleet flow) |

**Escalation flag.** Cluster-wide `roles/rolebindings: create` is a classic
privilege-escalation primitive — binding subjects inherit whatever a new
Role grants. Combined with namespaced `create` of arbitrary SA+Role+RB in
any namespace, a compromised controller SA with `fleetManager.enabled` can
mint itself (or any SA) arbitrary namespaced authority. This is accepted
risk and documented here; no mitigation is practical without an external
admission controller (OPA/Kyverno) restricting what Roles the fleet flow
may create.

#### 1d. ClusterRole — `controller/agent-cluster-rbac.yaml` (gated `clusterAdmin.enabled`)

| Resource | Granted | Notes |
|---|---|---|
| `clusterrolebindings` | get, list, watch, create, update, patch, delete with `resourceNames=[<coop-cluster-admin-SA-binding>]` | Correctly scoped by name |
| `clusterrolebindings` | **create** (no resourceNames) | **Escalation primitive** — `create` ignores `resourceNames` (it's a create verb, name isn't in the request path), so this rule allows creating *any* ClusterRoleBinding. A compromised controller SA can bind itself to `cluster-admin`. |
| `clusterroles` | get with `resourceNames=["cluster-admin"]` | Correctly scoped |

**Escalation note.** The second rule is structurally required (Helm needs to
create the binding on first install), but it's the chart's biggest remaining
trust boundary. Since this ClusterRole is only bound when
`clusterAdmin.enabled`, and the same toggle already puts a cluster-admin SA
in the ServiceAccountBinding subjects list, no new privilege is actually
gained beyond what `clusterAdmin.enabled=true` already grants. Document
loudly and leave.

### 2. Agent-pod SA (`gardenio-agents` *(coop)*, name = `{{release}}-agent`)

Defined in `controller/agent-rbac.yaml`. **Posture is ideal.** The Role's
`rules` default to `[]`; projects opt into narrow permissions via
`agents.agentServiceAccount.extraRules`. Combined with
`automountServiceAccountToken=false` (default, see prior audit §C3), agent
pods have no K8s API access. Good.

**No action needed.**

### 3. Coop cluster-admin SA (opt-in, `clusterAdmin.enabled`)

`controller/agent-cluster-rbac.yaml` binds `{{release}}-agent-cluster-admin`
directly to the built-in `cluster-admin` ClusterRole. This is **by design**:
projects that legitimately need cluster-admin (e.g. fleet bootstrap,
cluster-wide terraform) opt into this SA per-bead via the project's
`service_account` field. Only **this** SA is cluster-admin; the default
agent SA is not. That matches AUDIT-2026-04-18 §C1's recommended fix.

**No action needed**, but consider adding a `values.yaml` hard-block comment
that enabling `clusterAdmin.enabled` effectively grants controller SA the
ability to escalate (see §1d).

### 4. Slack-bridge SA (`{{release}}-slack-bridge`)

`slack-bridge/rbac.yaml` grants `traefik.io/middlewares: get, list, patch`.
Confirmed by `internal/bridge/gate.go` L60/L157. **Correctly minimal.**

**No `pods/exec`, `pods/attach`, `pods/portforward`, `pods/proxy`.** ✓

### 5. Token-rotator SA (`{{release}}-token-rotator`)

`token-rotator/rbac.yaml` grants exactly what the cronjob script patches:
`secrets: get, create, update, patch`, `configmaps: get, create, update, patch`,
`deployments, statefulsets: get, patch`. Matches the shell script verb-for-
verb. **Correctly minimal.**

### 6. vmagent SA

`vmagent/rbac.yaml`: `pods, endpoints, services: get, list, watch`.
Standard Prometheus-style scrape discovery. **Correctly minimal.**

### 7. grafana SA (sidecar)

`grafana/rbac.yaml`: `configmaps: get, list, watch`. Used by the
dashboard-loader sidecar. **Correctly minimal.**

### 8. pre-upgrade `adopt-pvcs` Job SA

`pre-upgrade-adopt-pvcs.yaml` grants `persistentvolumeclaims: get, list,
patch`. Matches the `kubectl label/annotate` calls in the Job. Correctly
minimal and auto-deleted via `helm.sh/hook-delete-policy`.

### 9. keycloak, postgres, nats, vmsingle, beads, coopmux, viewers, bridges, litellm, forwardauth, beads3d, fleet

Grep confirms **no `(Cluster)Role/(Cluster)RoleBinding/ServiceAccount`** is
declared by any of these templates beyond the chart-level entries already
listed. Only `roles.yaml` under `keycloak/` exists — it defines
`KeycloakRealmRole` CRDs (Keycloak-internal realm roles, unrelated to
Kubernetes RBAC).

**No action needed.**

---

## Escalation primitives — full sweep

Checked every rule for:

| Primitive | Present? | Where | Verdict |
|---|---|---|---|
| `roles/rolebindings: create/update/patch/delete/bind/escalate` | ✓ create/update/patch/delete (RB/Role) cluster-wide | fleet-manager ClusterRole | **accept** (fleet needs it; documented) |
| `clusterrolebindings: create` (unscoped) | ✓ | agent-cluster-manager ClusterRole | **accept** (only when `clusterAdmin.enabled`; documented §1d) |
| `certificatesigningrequests: approve` | ✗ | — | clean |
| `impersonate` | ✗ | — | clean |
| `nodes/proxy` | ✗ | — | clean |
| `pods/exec`, `pods/attach`, `pods/portforward`, `pods/proxy` | ✗ | — | clean (bridges *and* controller) |
| `pods/eviction` | ✗ | — | clean |
| `persistentvolumes: create/delete` | ✓ delete | pv-gc ClusterRole | **accept** (PV GC by design) |
| `secrets: *` wildcard | ✗ (all `get`/`create` only; no wildcards) | — | clean |

---

## Recommendations (in priority order)

### R1 — Fix the ExternalSecret verb bug (P1, correctness)

`controller/rbac.yaml` grants `externalsecrets: update` but the reconciler
only ever calls `Create`/`Get`/`List`/`Delete`. As written, orphan cleanup
(`reconciler.go` L228) will fail in restrictive clusters.

**Fix**: replace `update` → `delete`.

### R2 — Fix the per-agent-token Secrets gap (P1, correctness — only when `perAgentTokens=true`)

`controller/rbac.yaml` grants `secrets: get`; `agenttoken.go` calls
`Secrets.Create`/`Delete`. If `agents.perAgentTokens=true` without
`fleetManager.enabled` (which would incidentally grant cluster-wide
`secrets: create`), the flow breaks.

**Fix**: when `.Values.agents.perAgentTokens` is true, extend the
namespaced Role to `secrets: get, create, delete`. Pinning by
`resourceNames` is viable only if the token-secret names are derived
deterministically (they appear to be, based on `agenttoken.go` — if the
name prefix is known at template time, pin `resourceNames` to the pattern).

### R3 — Drop dead rules from `controller/rbac.yaml` (P2, hygiene)

Unused: `pods/log`, `events`, `services`, `deployments`. Removing these
shrinks blast radius and speeds up `rakkess -n gardenio --sa gardenio-agents`
reviews.

### R4 — Trim over-granted verbs on `controller/rbac.yaml` (P2, hygiene)

- `pods`: drop `watch`, `deletecollection`.
- `persistentvolumeclaims`: **add `update`** (needed for adoption path).
- `leases`: trim to `get, create, update` (drop list/watch/patch/delete).
- `persistentvolumes` in pv-gc CR: drop `get`.

### R5 — Trim fleet-manager ClusterRole (P2, hygiene)

- `namespaces`: trim `get, list, patch` (keep `create`, `delete`).
- `serviceaccounts`, `roles`, `rolebindings`: trim to `create`.
- `externalsecrets`: trim to `create`.

Tradeoff: losing `update/patch/delete` on the RBAC primitives means
`gb fleet update` (if/when introduced) must either re-issue via `delete +
create` or re-add the verb via a flag. Acceptable for an opt-in flow.

### R6 — Document cluster-admin bootstrap escalation (P3, posture)

Add a paragraph to `values.yaml` near `clusterAdmin.enabled` stating that
enabling it lets the controller SA create arbitrary ClusterRoleBindings
during Helm upgrades — same posture as a cluster-admin SA. Cross-link this
audit.

---

## Proposed trimmed Role YAMLs

### A. `controller/rbac.yaml` (replace rules block)

```yaml
rules:
  # Pod lifecycle: podmanager (create/delete), statusreporter+pvcgc (list).
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "create", "delete"]
  # PVC lifecycle: podmanager (create/get/update for helm-adoption), pvcgc (list/delete).
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["get", "list", "create", "update", "delete"]
  # Secret reads: credential materialisation for ratelimit probe, litellm aliases, subscribe path.
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
    {{- if .Values.agents.perAgentTokens }}
  # Per-agent token provisioning (agents.perAgentTokens=true).
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "delete"]
    {{- end }}
  # Leader election (coordination.k8s.io/v1 LeaseLock).
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
  # ExternalSecret CRDs: secretreconciler creates + reaps orphaned.
  - apiGroups: ["external-secrets.io"]
    resources: ["externalsecrets"]
    verbs: ["get", "list", "create", "delete"]
```

### B. `controller/pv-gc-rbac.yaml` (replace rules block)

```yaml
rules:
  - apiGroups: [""]
    resources: ["persistentvolumes"]
    verbs: ["list", "patch", "delete"]
```

### C. `controller/fleet-manager-rbac.yaml` (replace rules block)

```yaml
rules:
  # Namespace provisioning for fleets.
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["create", "delete"]
  # RBAC primitives per fleet namespace — create-only (operator must helm-
  # uninstall the fleet to rotate). Widen via a values flag if a `gb fleet
  # update` flow lands.
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    verbs: ["create"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["roles", "rolebindings"]
    verbs: ["create"]
  # Fleet token secrets.
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "create"]
  # Fleet ExternalSecrets.
  - apiGroups: ["external-secrets.io"]
    resources: ["externalsecrets"]
    verbs: ["create"]
```

### D. `controller/agent-cluster-rbac.yaml` — leave as-is

The `create` of arbitrary ClusterRoleBindings is structural (Helm install
semantics). Document it; don't attempt to trim.

### E. `controller/agent-rbac.yaml` — leave as-is

Empty rules by default is ideal. Don't regress.

---

## Verification commands

```bash
# Before applying the trims — baseline the current posture.
kubectl auth can-i --list --as=system:serviceaccount:gardenio:gardenio-agents -n gardenio
# (or, with `rakkess -n gardenio --sa gardenio-agents`)

# After applying the trims — rerun and diff.
# The ONLY diff should be the rows called out in §1a, §1b, §1c.

# Sanity-check no new verbs/resources were added to the controller since this audit.
cd gardenio/controller
grep -rn '\.CoreV1()\.\|\.AppsV1()\.\|\.RbacV1()\.\|\.CoordinationV1()\.\|\.Resource(' \
    --include='*.go' | grep -v _test.go
```

## Out of scope / follow-ups

- A `NetworkPolicy` sweep against the `gardenio/helm/gardenio/templates/networkpolicies/`
  directory. Related but separate bead recommended.
- Pod `SecurityContext` + `PodSecurityAdmission` label review (covered by
  the prior agent-image audit, but worth a periodic refresh).
- External tools mentioned in the bead (`rakkess`, `kubiscan`, `krane`,
  `kube-hunter`) — this audit was a static, source-cross-map pass. A live
  run against a gardenio-rwx cluster is a good complement and should produce
  the same gaps listed in R1–R5 (plus any deployed-but-unchartified RBAC).

---

*Audit by gardenio-audit-rbac-leastprivilege-fg0 against SHA `a5fa0817` on
branch `audit/rbac-least-privilege-ka-0000004571`.*
