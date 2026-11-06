# Per-project infra role: design

**Task:** gd-WFtxn8IpVL (P8) · **Plan:** gd-xiV5hyhPrG (agent self-service infra
access) · **Implemented by:** P9 (chart + role bead), P10 (escalation tier above) ·
**Status:** design, nothing here is deployed. Anchored on `origin/main` at
`eb768681` (2026-10-13). Lines drift; find code by the quoted text.

Owner direction (plan decision D2): *"we want a per project infra role for a
namespace of resources owned by the project if possible, which can escalate to
gardenio infra for e.g. connecting to our auth. Claimlayer is the first example."*

This doc says what a project's infra agent may do, how the chart expresses it, and
what P9 has to build that the chart cannot express today. ClaimLayer
(`claimlayer-demo`) is the worked example; `<project>` is the pattern.

## 1. The pattern in one table

| Piece | Where | Owned by | Created by |
|---|---|---|---|
| Namespace `<ns>` | cluster | the project's own deploy (ClaimLayer: `deploy/demo/k8s`) | the project, never the chart or an agent |
| Namespace label `gardenio.io/project=<project>`, PSA `enforce=restricted` | `<ns>` | operator | OPERATOR step (laptop-claude) |
| Role `<project>-infra` + RoleBinding | `<ns>` | chart, `projectNamespaceRbac.grants.<project>-infra` | helm |
| ResourceQuota, LimitRange, NetworkPolicies | `<ns>` | chart, same grant entry | helm (needs P9 chart work, §5) |
| ServiceAccount `<project>-infra` | ns `gardenio` | chart, new values key (P9 names it) | helm, default off |
| Role bead `type:role` `<project>-infra` | beads | gardenio | OPERATOR (admin-gated bead) |
| `type:spawn_policy` bead for the role | beads | gardenio | OPERATOR (admin-gated bead) |

Two rules the whole design hangs on:

1. **The Role is the only grant.** The SA has no ClusterRoleBinding and no rights in
   ns `gardenio`. Everything outside `<ns>` goes up as an escalation (§7), never
   as a wider Role.
2. **The chart never creates the namespace.** `templates/project-namespace-rbac.yaml`
   says so ("Namespace creation is deliberately NOT in scope") and fails the render
   for an enabled grant with no `namespace`.

## 2. Preconditions (checklist, in this order)

Nothing below is done by a crew. Items 1-3 are OPERATOR or owner steps.

- [ ] **P12 rolled.** P12 (gd-Gpvi7wld87: any agent bead can choose its pod's
  ServiceAccount) is merged **and** rolled before this SA is bound to anything. Today
  `podspec.go` honours `metadata["service_account"]` from the agent bead
  ("agent-level override via metadata"), so without P12 any agent could name
  `<project>-infra` and inherit the Role.
- [ ] **The namespace exists** and was created by the project's deploy. It is
  `claimlayer-demo` for ClaimLayer (hand-applied, no `gardenio.io/project` label as of
  2026-10-13 ~03:30Z).
- [ ] **Namespace carries `gardenio.io/project=<project>`** (OPERATOR applies it). The
  label is the inventory key: the grant's Role and RoleBinding carry the same label
  via `grants.<id>.project`, and `kubectl get roles,rolebindings -A -l gardenio.io/project`
  is how these are found.
- [ ] **Namespace carries PodSecurity `enforce=restricted`.** `claimlayer-demo` already
  does. The Role allows pod create; PSA restricted is what stops privileged pods,
  host namespaces and hostPath. Do not enable the grant on a namespace without it.
- [ ] **Every SA in `admissionPolicy.workloadServiceAccounts` is rightless.** The VAP
  (§4.1) denies Secret references and foreign SAs for the infra agent, but the SA it is
  allowed to run workloads as (default `default`) must have no RoleBinding or
  ClusterRoleBinding. The oauth2-proxy client and cookie secrets and the `ecr` pull
  secret stay unreadable only while that holds.
- [ ] **The namespace is not listed in `imdsDeny.namespaces`** (PR #270). The project
  egress policy in §6 denies IMDS by omission, and NetworkPolicies are additive:
  `imds-deny` allows `namespaceSelector: {}` plus all of `0.0.0.0/0` except IMDS, so
  listing the namespace there would silently re-open pod-to-pod egress and defeat the
  policy below.
- [ ] **The k3s apiserver is new enough for `ValidatingAdmissionPolicy`** (GA in 1.30);
  §4.1 depends on it. P9 confirms with `kubectl api-resources` or the node
  image version.

## 3. Grant

Use the existing `projectNamespaceRbac.grants.<project>-infra` entry. The template
renders one Role and one RoleBinding per entry, with `rules` and `subjects` taken
verbatim. Rules are explicit, never `*`:

```yaml
projectNamespaceRbac:
  enabled: true
  grants:
    claimlayer-infra:                  # <project>-infra
      enabled: true
      namespace: claimlayer-demo
      project: claimlayer
      rules:
        - apiGroups: ["apps"]
          resources: ["deployments", "replicasets", "statefulsets"]
          verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
        - apiGroups: ["batch"]
          resources: ["jobs", "cronjobs"]
          verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
        - apiGroups: [""]
          resources: ["pods", "configmaps", "services"]
          verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
        - apiGroups: [""]
          resources: ["pods/log"]
          verbs: ["get"]
        - apiGroups: ["", "events.k8s.io"]
          resources: ["events"]
          verbs: ["get", "list", "watch"]
        - apiGroups: ["networking.k8s.io"]
          resources: ["networkpolicies"]
          verbs: ["get", "list", "watch"]
      subjects:
        - kind: ServiceAccount
          name: claimlayer-infra
          namespace: gardenio
```

`pods/log` is `get` only (that is the only verb the subresource has). `deployments/scale`
and `pods/status` are not granted: a Deployment `patch` already changes replicas, and
nothing needs the status writes.

### What this role cannot do

Each line is an explicit **no** in the can-i matrix (§8):

- read, list or write **Secrets** (API level; see §4.1 for the pod-create caveat);
- `pods/exec`, `pods/attach`, `pods/portforward` (no shell into any pod, including its
  own);
- any RBAC object: `roles`, `rolebindings`, `clusterroles`, `clusterrolebindings`, and
  the `escalate`, `bind` and `impersonate` verbs;
- `serviceaccounts` writes, and `serviceaccounts/token` (no token minting);
- namespace writes, and anything cluster-scoped: nodes, PVs, CRDs, storage classes,
  webhook configurations;
- NetworkPolicy, ResourceQuota or LimitRange **writes** (read only on policies; the
  agent cannot loosen its own fence);
- `daemonsets`, `ingresses`, `persistentvolumeclaims`, `horizontalpodautoscalers`
  (not needed for the first project; adding one is a chart change, reviewed);
- anything in ns `gardenio`, `kube-system`, or any namespace other than `<ns>`.

## 4. Where the Role is weaker than it looks

### 4.1 Pod create is a Secret read: enforced with a ValidatingAdmissionPolicy

**Decision (owner, resolved): enforce with a `ValidatingAdmissionPolicy` (VAP).** The
residual-accept option was not taken. The rest of this section is the spec P9 builds.

Kubernetes RBAC is checked per API verb, not per effect. An agent that can `create pods`
in `<ns>` can set `env.valueFrom.secretKeyRef`, mount a `secret` volume, or set
`serviceAccountName` to any SA in `<ns>`, and then read the value from its own pod's logs
(`pods/log` is granted). So "no Secrets" is true of the API and false of the namespace.
PSA `restricted` does not help: secret volumes and secretKeyRefs are allowed by it.

In `claimlayer-demo` that exposes the oauth2-proxy client secret, the cookie secret and the
`ecr` pull secret, plus the `default` SA token, and, through the `ecr-token-refresh` SA,
a token that can edit Secrets in the namespace. It is inherent to "create pods in a
namespace", so the fix is at admission, not in the Role.

**Shape.** Native VAP (GA in 1.30, no Kyverno; the Kyverno set in
[`admission-policies.md`](admission-policies.md) is default off and agent-pod scoped).
Rendered with the grant, so removing the grant removes the fence with the Role.

| Part | Spec |
|---|---|
| Who it applies to | `matchConditions`: `request.userInfo.username == 'system:serviceaccount:gardenio:<project>-infra'`. Nobody else is affected: the project's own deploy, `infra-admin` and humans keep using Secrets. |
| Where | Binding `matchResources.namespaceSelector` on the automatic label `kubernetes.io/metadata.name = <ns>` (not the operator-applied `gardenio.io/project` label, so a missing label can neither disable the fence nor widen it). A policy error is then confined to `<ns>`. |
| What it matches | `CREATE` and `UPDATE` on `pods` and `pods/ephemeralcontainers` (v1); `deployments`, `replicasets`, `statefulsets` (apps/v1); `jobs`, `cronjobs` (batch/v1). `replicasets` must be listed: the Role grants direct ReplicaSet create, and pods made by a controller are created by the controller's identity, not the infra SA, so the **workload template** is the only place to check. |
| Actions | `failurePolicy: Fail`, `validationActions: ["Deny"]`. The SA has no running agent until the role bead exists (§10 step 4), so there is nothing to soak in Audit first. |
| Shape of the pod spec | One policy per shape (Pod: `object.spec`; Deployment, ReplicaSet, StatefulSet, Job: `object.spec.template.spec`; CronJob: `object.spec.jobTemplate.spec.template.spec`) rendered from one named template. Separate policies keep each CEL expression under the apiserver cost budget; a single ternary over all shapes may not. |

**Rules** (all must hold; `ps` is the pod spec; `containers` is `containers` plus
`initContainers`; each rule has its own `message` naming the fix):

1. **No Secret env.** No container with `env[].valueFrom.secretKeyRef` or
   `envFrom[].secretRef`.
2. **Volume allowlist, not denylist.** Every volume is one of `emptyDir`, `configMap`,
   `downwardAPI`, or `projected` whose sources are only `configMap` or `downwardAPI`.
   That denies `secret`, `projected.secret`, `projected.serviceAccountToken` (a token for
   the pod's SA), `csi` (secrets-store drivers), `persistentVolumeClaim`, `hostPath` and
   anything a later Kubernetes adds. Adding a PVC type is a reviewed chart change.
3. **Workload SA allowlist.** `ps.?serviceAccountName.orValue('default')` is in
   `admissionPolicy.workloadServiceAccounts` (default `["default"]`), and the deprecated
   `serviceAccount` field, if set, equals it. This excludes `ecr-token-refresh` and the
   oauth2-proxy SA. **Precondition:** each listed SA has no RoleBinding or
   ClusterRoleBinding granting anything (P9 checks with `kubectl auth can-i --list --as
   system:serviceaccount:<ns>:<sa>`).
4. **No token mount.** `ps.?automountServiceAccountToken.orValue(true) == false`: it must
   be set explicitly to `false`, because unset falls back to the SA's own setting.
5. **`imagePullSecrets` stay allowed.** The kubelet uses the secret to pull; the pod
   never receives its value. Residual: the infra agent can pull any image from the
   registry the `ecr` secret covers, which is the project's own repository scope.
6. If the CEL type-checker rejects mixing `containers` and `ephemeralContainers`, check
   ephemeral containers in a separate expression on the `pods/ephemeralcontainers` rule.

**Consequences, stated so P9 and the owner are not surprised:**

- The agent **cannot create or update a workload that carries any Secret reference**,
  including the project's own secret-bearing ones (oauth2-proxy). Not even `kubectl
  rollout restart` or an image bump: changing the container `command`, `args` or image
  of a container that already has a Secret in its env is itself a read (`echo
  $CLIENT_SECRET`), so an "unchanged refs are fine" exception would reopen the hole. A
  workload that needs a Secret is changed by the project's own deploy. That is the right
  outcome.
- The agent **can still delete** secret-bearing workloads (availability, not
  confidentiality; the project's deploy recreates them). A label-based ownership fence
  (`gardenio.io/managed-by: <project>-infra` required on `UPDATE` and `DELETE` via
  `oldObject`) would close that too. It is not in this decision and not in v1; file it
  if wanted.
- Workloads the agent creates run with no Kubernetes API token. One that needs the API
  is a project-deploy item.

**Chart (P9).** Values under the grant entry:

```yaml
admissionPolicy:
  enabled: true                        # required when the Role grants pods create
  workloadServiceAccounts: ["default"]
```

Render-time guard: a grant whose rules allow `create` on `pods` or any pod-bearing
workload and has `admissionPolicy.enabled` false **fails the render**, so the Secret
read cannot be shipped by omission. Objects, named `<project>-infra-no-secrets-<shape>`
(policy and binding each), carry the `gardenio.io/project` label.

**Tests (P9), each denial paired with a positive control:**

1. *Render* (helm tests): off by default and byte-identical; username in the
   `matchCondition`; `namespaceSelector` is `<ns>`; `failurePolicy: Fail`; `Deny`; the
   guard fails when `enabled: false`.
2. *CEL evaluation* (Go test in the chart test module, new `cel-go` test dependency):
   pull the expressions out of the rendered YAML and evaluate them against fixtures with
   `object` and `request.userInfo` set, for the Pod, Deployment and CronJob shapes.
   **First** a clean pod (emptyDir, no secret, SA `default`, automount false) is admitted
   and a request from another user skips the policy (matchCondition false); **then** deny
   cases: `secretKeyRef`, `secretRef`, `secret` volume, `projected` with a secret source,
   `projected` `serviceAccountToken`, `csi` volume, SA `ecr-token-refresh`, automount
   unset, automount `true`. Also run `cel-go`'s cost estimate against the list-size
   limits the apiserver uses.
3. *Live* (OPERATOR, §9): the apiserver is the final judge of cost and type-checking.
   `kubectl get validatingadmissionpolicy <name> -o jsonpath='{.status.typeChecking}'`
   shows no warnings, and `--dry-run=server` creates as the SA are denied and admitted
   as in §9.

**Services (CVE-2020-8554).** The Role allows `create`/`update`/`patch` on `services`, and
RBAC cannot restrict a field. A Service with `spec.externalIPs` that selects the agent's own
pod makes kube-proxy on every node route that IP:port to the pod, so the agent hijacks
traffic cluster-wide and reads it through `pods/log`; the live cluster has neither the
`DenyServiceExternalIPs` admission plugin nor a policy on Services. A fourth policy,
`<project>-infra-service-exposure` (policy and binding), matches `services` on CREATE and
UPDATE for the infra SA only and denies: a `spec.type` other than `ClusterIP` (also closes
LoadBalancer through ServiceLB, which opens host ports on the public nodes, and NodePort),
any `spec.externalIPs`, and any `spec.externalName`. It reads the same fields in the CEL test
(`TestProjectInfraRole_ServiceVAPCEL`: plain, type-unset and headless Services admitted
first, then each denial, plus a scope control for other users).

Rollout note: the RoleBinding and the VAP land in one `helm upgrade` with no ordering
guarantee, but no pod can run as `<project>-infra` until the role bead exists (§10 step
4), so there is no exposure window.

### 4.2 The spawn policy cannot yet say "requesters from `<project>`"

The brief wants the `spawn_policy` to auto-approve "only requesters whose project is
`<project>`". The evaluator cannot express that today.
`kbeads/internal/server/spawn_policy.go` matches on three keys: `project`,
`namespace`, `requester_role`. Reading `evaluateSpawnEscalation`:

- `project` is `projectFromFields(spec.Fields)`: the project of the **agent bead being
  spawned**, a field the requester chooses. It is not the requester's project.
- `namespace` is the requester's `pod_namespace`, which is `gardenio` for every agent.
- `requester_role` is the requester's role, shared across projects.

And `authorizeNonHumanSpawn` looks the role up by `Title` alone, so nothing ties a
`<project>-infra` spawn to the project named on the new bead. A gardenio crew that
requests role `claimlayer-infra` with `project=claimlayer` matches an `auto` rule
keyed on `when.project: claimlayer`.

So the cap cannot be a statement about the requester until the rule can see it.
**P9 must do one of:**

1. **(Recommended) add a `requester_project` key** to `spawnPolicyRuleWhen`.
   `resolveRequesterContext` already loads the requester's agent bead; its `project` field
   is one more struct member. Runtime authz code, so a draft PR with `merge-desk:hold`
   and a merge window. Tests: a requester in `<project>` matches; a requester in another
   project falls through (the positive control).
2. **Until then, ship the policy with `mode: human` only.** Spawning is then approved by
   an OIDC human each time. Correct, slower, and no code change.

The cap when `auto` is eventually enabled: `max_per_day: 4`, `max_concurrent: 1`,
`when: {project: <project>, requester_project: <project>}`, with a trailing `human`
rule so anything else (a different project, an over-cap request) waits for a person
rather than being denied, because a spawn policy with no matching rule **denies and
closes** the escalation. These numbers are tighter than the read-only `infra` role's
(6/day, 2 concurrent) because this role writes. P9 may propose different numbers in
its PR; they are not a decision for the owner.

`infra-admin` is separate and never auto: no `auto` rule, ever.

## 5. Identity and what the chart must add

**Identity.** A dedicated ServiceAccount `<project>-infra` in ns `gardenio` (where agent
pods run). `automountServiceAccountToken: false` on the SA, as in
`templates/controller/infra-reader-rbac.yaml`; the token is mounted into a pod only
because the role bead sets `needs_k8s_api=true` (`podmanager/manager.go`: `automount :=
spec.NeedsK8sAPI`). The RoleBinding subject is `gardenio/<project>-infra`. There is no
ClusterRoleBinding, and the SA is bound to nothing in ns `gardenio`.

PR #284 (open) makes a role bead with `needs_k8s_api` and `service_account=gardenio-agent`
a hard reject, and warns when `service_account` is empty. The `<project>-infra` role sets
a dedicated SA, so it passes.

**Gap check: can the grant entry express items 2 and 5?** Item 2 (Role + RoleBinding):
**yes, as is**, via `rules` and `subjects`. Item 5 (guardrails): **no.** The template
renders only a Role and a RoleBinding. P9 must add:

| Need | Today | P9 adds |
|---|---|---|
| ServiceAccount `<project>-infra` in ns `gardenio` | none | a new top-level values key next to `projectNamespaceRbac` (P9 names it), rendering one SA per listed project, default off. Same shape as `agents.infraReader` |
| ResourceQuota in `<ns>` | none per grant (`resourcequota.yaml` renders one quota for the release namespace) | `grants.<id>.resourceQuota.hard`, rendered in `<ns>` |
| LimitRange in `<ns>` | none per grant (`limitrange.yaml` is release-namespace only) | `grants.<id>.limitRange.limits` |
| NetworkPolicies in `<ns>` | none | `grants.<id>.networkPolicies` (egress, IMDS) |
| VAP + binding (§4.1, chosen) | none | `grants.<id>.admissionPolicy` (`enabled`, `workloadServiceAccounts`), one policy and binding per pod-spec shape, plus the render-time guard |

Per-object `create` flags and the render-time `fail` guards should be copied for each
new object, so a half-configured grant fails loudly. Every new object carries the
`gardenio.io/project` label. Default render stays byte-identical when the values are off
(the existing chart tests assert this for `projectNamespaceRbac`; keep that).

## 6. Guardrails inside the namespace

All owned by the project's grant entry, so removing the grant removes the fence with the
Role. Sizes are for ClaimLayer's current footprint (two Deployments at 10m/32Mi,
128Mi limit); P9 sets them in the values overlay, not in the template.

- **ResourceQuota** `<project>-infra`: `pods`, `requests.cpu`, `requests.memory`,
  `limits.memory`. Suggested start: `pods: "10"`, `requests.cpu: "1"`,
  `requests.memory: 1Gi`, `limits.memory: 2Gi`. The quota is what bounds what a
  compromised infra agent can schedule.
- **LimitRange** `<project>-infra` with `default` and `defaultRequest`
  (e.g. 100m / 128Mi request, 256Mi limit). Required, not optional: a
  ResourceQuota on `limits.memory` rejects any pod that sets no limit, and the
  project's existing restricted pods set none for some containers. The LimitRange
  fills the defaults so they still schedule.
- **IMDS deny.** Egress to `169.254.169.254/32` (and `fd00:ec2::254/128`) must not be
  reachable. The policy below achieves this by never allowing it; see the precondition
  about not listing `<ns>` in `imdsDeny.namespaces`.
- **Egress policy** selecting every pod in `<ns>` (`podSelector: {}`, `policyTypes:
  [Egress]`). Allows exactly:
  1. DNS to `kube-system` pods `k8s-app=kube-dns` (the Service is 10.43.0.10), UDP and
     TCP 53;
  2. the Keycloak runtime Service in ns `gardenio` on TCP 8080
     (selector `app.kubernetes.io/component=keycloak-runtime,
     app.kubernetes.io/instance=gardenio, app.kubernetes.io/name=gardenio`), because
     oauth2-proxy calls `gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080` for
     redeem, jwks and userinfo;
  3. pod-to-pod **within `<ns>`** (`podSelector: {}`, same namespace only); oauth2-proxy
     reaches the app through this;
  4. HTTPS (TCP 443) to `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`,
     `192.168.0.0/16`, `169.254.0.0/16`, `100.64.0.0/10`. This is the same exclusion list
     the controller egress policy uses (`default-deny-egress.yaml`), and the exclusion is
     what keeps pod CIDR 10.42.0.0/16 and service CIDR 10.43.0.0/16 out.

  There is no rule for other namespaces, so pod-to-pod into any other namespace
  (including `gardenio` apart from Keycloak:8080) is denied. Item 2 must be a
  namespace+pod selector pair, not a CIDR.
- **Existing ingress policies stay.** `claimlayer-demo-from-oauth2-proxy` and
  `oauth2-proxy-from-public-edge` are ingress-only and the project's; the egress policy
  here adds a second axis and does not touch them. Because NetworkPolicy is additive per
  direction, adding an egress policy does not change ingress.
- **Test with a positive control.** From a pod in `<ns>`: DNS resolves, Keycloak:8080
  answers, an HTTPS egress to a public host connects (these pass); a connection to a pod
  in another namespace, to 169.254.169.254 and to the kube-apiserver on 10.43.0.1:443
  fails. Without the three passing cases, the three failing ones prove only a broken
  network.

Unmanaged `ecr-token-refresh` (Role and RoleBinding in `<ns>`, subject
`gardenio/ecr-token-refresh`) is the project's, not this grant's. The infra Role never
reads or edits it.

## 7. Escalation upward (never a wider Role)

Anything the project's infra agent cannot do inside `<ns>` is a request to the gardenio
tier, filed with `gb escalation create` and executed by an operator or the `infra-admin`
agent. The chain, the fields a request carries and the ClaimLayer worked example are in
[`project-infra-escalation-chain.md`](project-infra-escalation-chain.md) (P10):

- a **Keycloak client** or redirect URI for the project;
- a **public-edge vhost** or route (the `oauth2-proxy-from-public-edge` ingress path);
- changes to the project's own `grants.<project>-infra` entry (quota size, a new
  resource in the Role): a reviewed chart PR;
- a Secret value, a new namespace label, a new IRSA role.

If an escalation is wanted often enough that the team wants a wider Role, that is a chart
PR to this document first, not a runtime grant.

## 8. Role bead and spawn policy

Both are admin-gated beads, written by the OPERATOR step with an AUDIT RECORD comment.

**Role bead** `type:role`, title `<project>-infra`:

| Field | Value |
|---|---|
| `privileged` | `true` |
| `needs_k8s_api` | `true` |
| `service_account` | `<project>-infra` |
| CPU / memory | the same as the `infra` role: 500m / 1Gi request, 1500m / 2Gi limit |
| labels | `privileged-role`, `project:<project>` |

`privileged=true` is what routes a spawn through the escalation (`authorizeNonHumanSpawn`
→ `roleIsPrivileged`, role keyed by `Title`). `service_account` needs P12 to be
rolled (§2).

**Spawn policy** `type:spawn_policy` for role `<project>-infra`: see §4.2 for the cap and
the `requester_project` gap. Start in `mode: human`. The evaluator is currently dark:
`BEADS_SPAWN_POLICY_ENABLED` is unset on `gardenio-beads`, so with no policy bead or flag
every spawn of the role waits for a human regardless.

## 9. Can-i matrix (P9's test)

P9 runs these against the live SA after rollout, from the OPERATOR's shell (crews do not
run `kubectl` against the live cluster):

`kubectl auth can-i <verb> <resource> -n <ns> --as system:serviceaccount:gardenio:<project>-infra`

Every "no" is paired with the "yes" rows, so a broken `--as` or a dead cluster cannot read
as a pass: the first row must print `yes` before any `no` is believed.

**Yes** (in `<ns>`):

| Resource | Verbs |
|---|---|
| `deployments.apps`, `replicasets.apps`, `statefulsets.apps` | get, list, watch, create, update, patch, delete |
| `jobs.batch`, `cronjobs.batch` | get, list, watch, create, update, patch, delete |
| `pods`, `configmaps`, `services` | get, list, watch, create, update, patch, delete |
| `pods/log` | get |
| `events` | get, list, watch |
| `networkpolicies.networking.k8s.io` | get, list, watch |

**No** (in `<ns>`):

| Resource | Verbs |
|---|---|
| `secrets` | get, list, watch, create, update, patch, delete |
| `pods/exec`, `pods/attach`, `pods/portforward` (subresources, see note below) | create, get |
| `roles`, `rolebindings` (rbac) | get, list, create, update, patch, delete, bind, escalate |
| `clusterroles`, `clusterrolebindings` | get, list, create, bind, escalate |
| `serviceaccounts` | create, update, patch, delete |
| `serviceaccounts/token` | create |
| `*` (impersonate) | impersonate |
| `networkpolicies`, `resourcequotas`, `limitranges` | create, update, patch, delete |
| `daemonsets.apps`, `ingresses.networking.k8s.io`, `persistentvolumeclaims` | get, create |

**Subresources need `--subresource`.** In `kubectl auth can-i`, `TYPE/NAME` means a
resource *name*, so `can-i create pods/exec` asks whether the SA may create a pod *named*
`exec`, which prints `yes` and looks like an exec grant. Write
`kubectl auth can-i create pods --subresource=exec` (likewise `attach`, `portforward`, and
`get pods --subresource=log`).

**No, everywhere else** (every "yes" row above, repeated outside `<ns>`):

- ns `gardenio`: `pods`, `secrets`, `configmaps`, `deployments.apps`, `pods/log`;
- ns `kube-system`: the same;
- one other project namespace, if one exists;
- cluster scope: `namespaces` (get, list, create, delete), `nodes` (get, list),
  `persistentvolumes` (get, list), `clusterroles` (list).

`kubectl auth can-i --list -n <ns>` must show no rule outside the Role. In `gardenio`
and `kube-system` it must show no resource rules beyond the discovery and self-review
entries every authenticated identity gets.

**Admission (§4.1):** run `kubectl create --dry-run=server --as
system:serviceaccount:gardenio:<project>-infra -n <ns> -f <fixture>` (fixtures from the
P9 CEL test). The first fixture, a pod with an emptyDir, no secret reference, SA
`default` and `automountServiceAccountToken: false`, must be **admitted**; only then are
the denials believed: a pod with `secretKeyRef`; one with `secretRef`; one mounting a
`secret` volume; one with a projected `serviceAccountToken`; one running as
`ecr-token-refresh`; one with automount unset; the same secret reference inside a
Deployment template and a CronJob template; and a direct ReplicaSet with a
`secretKeyRef`. As a different identity (the operator), the secret-bearing pod fixture is
admitted by the VAP (it does not apply), which proves the fence is scoped to the infra SA.

**Admission, Services (§4.1).** Same `--dry-run=server --as ...` form, positive control
first. A plain `type: ClusterIP` Service with a selector must be **admitted**. Then each of
these must be **DENIED** with the `service-exposure` message:

| Fixture | Why it is denied |
|---|---|
| `spec.externalIPs: ["10.43.0.10"]` (selector on a pod in `<ns>`; the kube-dns ClusterIP, any routable IP works) | CVE-2020-8554; the s9 canary |
| `spec.type: LoadBalancer` | ServiceLB opens host ports on the public nodes |
| `spec.type: NodePort` | host ports |
| `spec.type: ExternalName`, `externalName: kbeads.gardenio.svc` | redirects cluster DNS |

Do not use a link-local address such as `169.254.169.254` as the canary: API validation
rejects it before any admission policy runs, so the denial would not come from the policy.
As the operator, the `externalIPs` fixture is **admitted** by this policy (it does not
apply), so a denial is the policy and not a broken fixture. Delete anything the operator
control created (`--dry-run=server` creates nothing).

**Live-proof note (2026-10-14).** A test Job in `<ns>` stays `Pending` ("untolerated taint")
unless it mirrors the demo Deployment's `nodeSelector` (spot) and the `gardenio.agent=true`
toleration; with both it schedules and completes in seconds.

## 10. Rollout order

1. **P12 rolled** (merged and applied). Nothing below starts before it.
2. **Chart grant** (P9): SA, Role, RoleBinding, quota, LimitRange, policies and the VAP. Draft PR with `merge-desk:hold`, one merge-window escalation, helm
   upgrade by the OPERATOR. All values default off.
3. **Namespace label** `gardenio.io/project=<project>` (OPERATOR), plus the check that
   PSA `enforce=restricted` is set.
4. **Role bead, then spawn policy bead** (OPERATOR, guarded SQL, AUDIT RECORD comment).
   Spawn policy starts in `human` mode.
5. **Live proof** (OPERATOR): the can-i matrix (§9), the network positive and negative
   controls (§6), a real spawn of `<project>-infra` through the escalation, and one
   workload create and delete in `<ns>` by that agent.

Rollback: step 5 → close the agent; 4 → close the role bead (nothing can spawn it);
3 → remove the label; 2 → `grants.<id>.enabled: false` (removes Role, RoleBinding and
guardrails together, SA last); 1 is not rolled back. Revert this doc to drop the design.

## 11. Out of scope

- Gas City projects (`gc-dogfood`, `k8s-live`, `gascity`).
- The gardenio admin infra role `infra-admin` (P6): full cluster control, never
  auto-approved, every spawn approved by an OIDC human.
- PR #270 (IMDS deny) and PR #273 (per-agent tokens): this design depends on neither,
  and only constrains #270 by the `imdsDeny.namespaces` precondition above.
- Creating the project namespace, project Secrets, or Keycloak clients.

## 12. Implementation notes (P9, gd-IYY1dxukfR)

Built as designed, with these choices the sections above left open:

- **Chart.** `templates/project-infra-role.yaml` renders, per grant: ResourceQuota and
  LimitRange (`resourceQuota.hard`, `limitRange.limits`), and for `agentInfra.enabled`
  the two NetworkPolicies and the VAP set (`<name>-no-secrets-{pod,workload,cronjob}`, plus `<name>-service-exposure`, §4.1).
  The SA comes from the new top-level `projectInfraServiceAccounts`.
- **Two egress policies, not an "IMDS-deny" policy.** NetworkPolicies are additive, so a
  policy that allows `0.0.0.0/0` except IMDS would open everything the others close. The
  allow-set is split into `<name>-egress-internal` (DNS, Keycloak runtime :8080,
  same-namespace pods) and `<name>-egress-https` (443 with the private ranges, IMDS and
  CGNAT excluded). IMDS is denied by omission.
- **Guards.** `agentInfra.enabled` fails the render without `project`, `resourceQuota.hard`
  or `limitRange.limits`, and a grant whose rules can create or change pod-bearing
  workloads fails without `agentInfra.admissionPolicy.enabled`.
- **Release-deployer.** helm reads every object it owns, and escalation prevention lets the
  deployer write the infra Role only if it holds it. `release-deployer/rbac.yaml` therefore
  renders a deployer Role/RoleBinding in each `agentInfra` namespace (the infra rules
  verbatim, plus named access to the quota, LimitRange, policies, Role and RoleBinding) and
  adds the VAP names to its ClusterRole. **The first install of that Role needs an operator
  roll**, like the keycloak-operator namespace one; after that autonomous rolls work. Also,
  changing `deploy-values/gardenio-k3s.yaml` parks the autonomous deployer until an
  operator rolls.
- **Rollout-restart caveat.** The VAP checks the whole new object on UPDATE. A restart of
  a workload that already has `automountServiceAccountToken` unset, a non-`default` SA or a
  Secret reference is denied. The OPERATOR checks the live `claimlayer-demo` Deployment
  (`kubectl -n claimlayer-demo get deploy claimlayer-demo -o jsonpath='{.spec.template.spec.automountServiceAccountToken}'`)
  before the live proof; if it is unset, the project's own deploy sets it to `false`.
- **Tests.** `tests/project_infra_role_test.go` evaluates the rendered CEL with `cel-go`
  (clean workload admitted first, then each denial, plus a scope control for other users).
  The apiserver stays the final judge of cost and type-checking: check
  `kubectl get validatingadmissionpolicy <name> -o jsonpath='{.status.typeChecking}'`.

### Operator review of PR #346: N1-N4

- **N1 (fixed): Service `externalIPs`, CVE-2020-8554.** The fourth VAP,
  `<name>-service-exposure` (§4.1), denies a non-`ClusterIP` type, any `externalIPs` and any
  `externalName` for the infra SA on CREATE and UPDATE. The release-deployer ClusterRole
  names it with the others. The s9 server dry-run fixture (§9) must be denied.
- **N2 (narrowed, residual risk accepted): the release-deployer's Role in `<ns>`.** It
  copies the infra rules without the VAP (the VAP matches only the infra SA), so the
  deployer can create pods there that mount the ECR and oauth2-proxy Secrets, and it holds
  `update`/`patch`/`delete` on the VAPs, so a merged chart change can weaken the fence. It is
  bounded to the namespace and to reviewed chart changes, and a merge is `merge-desk:hold`.
  **`create` on `roles` and `rolebindings` is dropped** (`create` cannot be name-restricted,
  and a new RoleBinding can bind any Role the deployer holds to any subject). The cost: the
  first install of the infra Role/RoleBinding and any NEW Role or RoleBinding added to the
  namespace later is an operator roll, which the first install already was.
  `TestProjectInfraRole_DeployerCannotCreateRolesOrBindings` pins it (with a control that the
  `create` scan still sees `networkpolicies`).
- **N3 (closed by N1):** LoadBalancer through ServiceLB and NodePort are denied by the
  `type == ClusterIP` rule.
- **N4 (accepted, in §4.1/§6 terms):** a pod labelled `app.kubernetes.io/name=oauth2-proxy`
  joins `claimlayer-demo-auth`'s ingress policy and so sees the demo users' cookies. The
  Role can create pods, and the project's ingress policies select on that label. This is
  accepted as "operating the demo": the agent already runs the demo's workloads, and the
  demo's users are the owner's own. It is not mitigated by the chart; the project's ingress
  policies are the project's (§6).
- **Live precondition: the demo Deployment's `automountServiceAccountToken`.** The VAP checks
  the whole new object on UPDATE, so `rollout restart` of a Deployment that leaves it unset
  is denied. The ClaimLayer repo sets `automountServiceAccountToken: false` on the
  `claimlayer-demo` pod template (and on the namespace's `default` ServiceAccount) in
  `deploy/demo/k8s`; that PR must be applied before the live proof. Check with the jsonpath
  in the rollout-restart caveat above.
